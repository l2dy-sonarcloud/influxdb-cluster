package meta

import (
	"fmt"
	"io"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/hashicorp/raft"
	internal "github.com/influxdata/influxdb/services/meta/internal"
	"github.com/influxdata/influxql"
	"go.uber.org/zap"
)

// storeFSM represents the finite state machine used by Store to interact with Raft.
type storeFSM store

func (fsm *storeFSM) Apply(l *raft.Log) interface{} {
	var cmd internal.Command
	if err := proto.Unmarshal(l.Data, &cmd); err != nil {
		panic(fmt.Errorf("cannot marshal command: %x", l.Data))
	}

	// Lock the store.
	s := (*store)(fsm)
	s.mu.Lock()
	defer s.mu.Unlock()

	err := func() interface{} {
		switch cmd.GetType() {
		case internal.Command_RemovePeerCommand:
			return fsm.applyRemovePeerCommand(&cmd)
		case internal.Command_CreateNodeCommand:
			// create node was in < 0.10.0 servers, we need the peers
			// list to convert to the appropriate data/meta nodes now
			peers, err := s.raftState.peers()
			if err != nil {
				return err
			}
			return fsm.applyCreateNodeCommand(&cmd, peers)
		case internal.Command_DeleteNodeCommand:
			return fsm.applyDeleteNodeCommand(&cmd)
		case internal.Command_CreateDatabaseCommand:
			return fsm.applyCreateDatabaseCommand(&cmd)
		case internal.Command_DropDatabaseCommand:
			return fsm.applyDropDatabaseCommand(&cmd)
		case internal.Command_CreateRetentionPolicyCommand:
			return fsm.applyCreateRetentionPolicyCommand(&cmd)
		case internal.Command_DropRetentionPolicyCommand:
			return fsm.applyDropRetentionPolicyCommand(&cmd)
		case internal.Command_UpdateRetentionPolicyCommand:
			return fsm.applyUpdateRetentionPolicyCommand(&cmd)
		case internal.Command_CreateShardGroupCommand:
			return fsm.applyCreateShardGroupCommand(&cmd)
		case internal.Command_DeleteShardGroupCommand:
			return fsm.applyDeleteShardGroupCommand(&cmd)
		case internal.Command_CreateContinuousQueryCommand:
			return fsm.applyCreateContinuousQueryCommand(&cmd)
		case internal.Command_DropContinuousQueryCommand:
			return fsm.applyDropContinuousQueryCommand(&cmd)
		case internal.Command_CreateSubscriptionCommand:
			return fsm.applyCreateSubscriptionCommand(&cmd)
		case internal.Command_DropSubscriptionCommand:
			return fsm.applyDropSubscriptionCommand(&cmd)
		case internal.Command_CreateUserCommand:
			return fsm.applyCreateUserCommand(&cmd)
		case internal.Command_DropUserCommand:
			return fsm.applyDropUserCommand(&cmd)
		case internal.Command_UpdateUserCommand:
			return fsm.applyUpdateUserCommand(&cmd)
		case internal.Command_SetPrivilegeCommand:
			return fsm.applySetPrivilegeCommand(&cmd)
		case internal.Command_SetAdminPrivilegeCommand:
			return fsm.applySetAdminPrivilegeCommand(&cmd)
		case internal.Command_SetDataCommand:
			return fsm.applySetDataCommand(&cmd)
		case internal.Command_UpdateNodeCommand:
			return fsm.applyUpdateNodeCommand(&cmd)
		case internal.Command_CreateMetaNodeCommand:
			return fsm.applyCreateMetaNodeCommand(&cmd)
		case internal.Command_DeleteMetaNodeCommand:
			return fsm.applyDeleteMetaNodeCommand(&cmd)
		case internal.Command_SetMetaNodeCommand:
			return fsm.applySetMetaNodeCommand(&cmd)
		case internal.Command_CreateDataNodeCommand:
			return fsm.applyCreateDataNodeCommand(&cmd)
		case internal.Command_DeleteDataNodeCommand:
			return fsm.applyDeleteDataNodeCommand(&cmd)
		case internal.Command_UpdateDataNodeCommand:
			return fsm.applyUpdateDataNodeCommand(&cmd)
		case internal.Command_DropShardCommand:
			return fsm.applyDropShardCommand(&cmd)
		case internal.Command_TruncateShardGroupsCommand:
			return fsm.applyTruncateShardGroupsCommand(&cmd)
		case internal.Command_PruneShardGroupsCommand:
			return fsm.applyPruneShardGroupsCommand(&cmd)
		case internal.Command_CopyShardOwnerCommand:
			return fsm.applyCopyShardOwnerCommand(&cmd)
		case internal.Command_RemoveShardOwnerCommand:
			return fsm.applyRemoveShardOwnerCommand(&cmd)
		default:
			panic(fmt.Errorf("cannot apply command: %x", l.Data))
		}
	}()

	// Copy term and index to new metadata.
	fsm.data.Term = l.Term
	fsm.data.Index = l.Index

	// signal that the data changed
	close(s.dataChanged)
	s.dataChanged = make(chan struct{})

	return err
}

func (fsm *storeFSM) applyRemovePeerCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_RemovePeerCommand_Command)
	v := ext.(*internal.RemovePeerCommand)

	addr := v.GetAddr()

	// Only do this if you are the leader
	if fsm.raftState.isLeader() {
		// Remove that node from the peer
		fsm.logger.Info("removing peer", zap.String("addr", addr))
		if err := fsm.raftState.removePeer(addr); err != nil {
			fsm.logger.Info("error removing peer", zap.Error(err))
		}
	}

	return nil
}

func (fsm *storeFSM) applyCreateNodeCommand(cmd *internal.Command, peers []string) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CreateNodeCommand_Command)
	v := ext.(*internal.CreateNodeCommand)

	// Copy data and update.
	other := fsm.data.Clone()

	// CreateNode is a command from < 0.10.0 clusters. Every node in
	// those clusters would be a data node and only the nodes that are
	// in the list of peers would be meta nodes
	isMeta := false
	for _, p := range peers {
		if v.GetHost() == p {
			isMeta = true
			break
		}
	}

	if isMeta {
		if err := other.CreateMetaNode(v.GetHost(), v.GetHost()); err != nil {
			return err
		}
	}

	// Get the only meta node
	if len(other.MetaNodes) == 1 {
		metaNode := other.MetaNodes[0]

		if err := other.setDataNode(metaNode.ID, v.GetHost(), v.GetHost()); err != nil {
			return err
		}
	} else {
		if err := other.CreateDataNode(v.GetHost(), v.GetHost()); err != nil {
			return err
		}
	}

	// If the cluster ID hasn't been set then use the command's random number.
	if other.ClusterID == 0 {
		other.ClusterID = v.GetRand()
	}

	fsm.data = other
	return nil
}

// applyUpdateNodeCommand was in < 0.10.0, noop this now
func (fsm *storeFSM) applyUpdateNodeCommand(cmd *internal.Command) interface{} {
	return nil
}

func (fsm *storeFSM) applyUpdateDataNodeCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_UpdateDataNodeCommand_Command)
	v := ext.(*internal.UpdateDataNodeCommand)

	// Copy data and update.
	other := fsm.data.Clone()

	node := other.DataNode(v.GetID())
	if node == nil {
		return ErrNodeNotFound
	}

	node.Addr = v.GetHTTPAddr()
	node.TCPAddr = v.GetTCPAddr()

	fsm.data = other
	return nil
}

// applyDeleteNodeCommand is from < 0.10.0. no op for this one
func (fsm *storeFSM) applyDeleteNodeCommand(cmd *internal.Command) interface{} {
	return nil
}

func (fsm *storeFSM) applyCreateDatabaseCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CreateDatabaseCommand_Command)
	v := ext.(*internal.CreateDatabaseCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.CreateDatabase(v.GetName()); err != nil {
		return err
	}

	s := (*store)(fsm)
	if rpi := v.GetRetentionPolicy(); rpi != nil {
		if err := other.CreateRetentionPolicy(v.GetName(), &RetentionPolicyInfo{
			Name:               rpi.GetName(),
			ReplicaN:           int(rpi.GetReplicaN()),
			Duration:           time.Duration(rpi.GetDuration()),
			ShardGroupDuration: time.Duration(rpi.GetShardGroupDuration()),
		}, true); err != nil {
			if err == ErrRetentionPolicyExists {
				return ErrRetentionPolicyConflict
			}
			return err
		}
	} else if s.config.RetentionAutoCreate {
		// Read node count.
		// Retention policies must be fully replicated.
		replicaN := len(other.DataNodes)
		if replicaN > MaxAutoCreatedRetentionPolicyReplicaN {
			replicaN = MaxAutoCreatedRetentionPolicyReplicaN
		} else if replicaN < 1 {
			replicaN = 1
		}

		// Create a retention policy.
		rpi := NewRetentionPolicyInfo(DefaultRetentionPolicyName)
		rpi.ReplicaN = replicaN
		rpi.Duration = DefaultRetentionPolicyDuration
		if err := other.CreateRetentionPolicy(v.GetName(), rpi, true); err != nil {
			return err
		}
	}

	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyDropDatabaseCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_DropDatabaseCommand_Command)
	v := ext.(*internal.DropDatabaseCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.DropDatabase(v.GetName()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyCreateRetentionPolicyCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CreateRetentionPolicyCommand_Command)
	v := ext.(*internal.CreateRetentionPolicyCommand)
	pb := v.GetRetentionPolicy()

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.CreateRetentionPolicy(v.GetDatabase(),
		&RetentionPolicyInfo{
			Name:               pb.GetName(),
			ReplicaN:           int(pb.GetReplicaN()),
			Duration:           time.Duration(pb.GetDuration()),
			ShardGroupDuration: time.Duration(pb.GetShardGroupDuration()),
		}, v.GetDefault()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyDropRetentionPolicyCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_DropRetentionPolicyCommand_Command)
	v := ext.(*internal.DropRetentionPolicyCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.DropRetentionPolicy(v.GetDatabase(), v.GetName()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyUpdateRetentionPolicyCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_UpdateRetentionPolicyCommand_Command)
	v := ext.(*internal.UpdateRetentionPolicyCommand)

	// Create update object.
	rpu := RetentionPolicyUpdate{Name: v.NewName}
	if v.Duration != nil {
		value := time.Duration(v.GetDuration())
		rpu.Duration = &value
	}
	if v.ReplicaN != nil {
		value := int(v.GetReplicaN())
		rpu.ReplicaN = &value
	}
	if v.ShardGroupDuration != nil {
		value := time.Duration(v.GetShardGroupDuration())
		rpu.ShardGroupDuration = &value
	}

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.UpdateRetentionPolicy(v.GetDatabase(), v.GetName(), &rpu, v.GetDefault()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyCreateShardGroupCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CreateShardGroupCommand_Command)
	v := ext.(*internal.CreateShardGroupCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.CreateShardGroup(v.GetDatabase(), v.GetPolicy(), time.Unix(0, v.GetTimestamp())); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyDeleteShardGroupCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_DeleteShardGroupCommand_Command)
	v := ext.(*internal.DeleteShardGroupCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.DeleteShardGroup(v.GetDatabase(), v.GetPolicy(), v.GetShardGroupID()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyDropShardCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_DropShardCommand_Command)
	v := ext.(*internal.DropShardCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	other.DropShard(v.GetID())
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyTruncateShardGroupsCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_TruncateShardGroupsCommand_Command)
	v := ext.(*internal.TruncateShardGroupsCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	other.TruncateShardGroups(time.Unix(0, v.GetTimestamp()))
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyPruneShardGroupsCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_PruneShardGroupsCommand_Command)
	_ = ext.(*internal.PruneShardGroupsCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	other.PruneShardGroups()
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyCopyShardOwnerCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CopyShardOwnerCommand_Command)
	v := ext.(*internal.CopyShardOwnerCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	other.CopyShardOwner(v.GetID(), v.GetNodeID())
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyRemoveShardOwnerCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_RemoveShardOwnerCommand_Command)
	v := ext.(*internal.RemoveShardOwnerCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	other.RemoveShardOwner(v.GetID(), v.GetNodeID())
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyCreateContinuousQueryCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CreateContinuousQueryCommand_Command)
	v := ext.(*internal.CreateContinuousQueryCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.CreateContinuousQuery(v.GetDatabase(), v.GetName(), v.GetQuery()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyDropContinuousQueryCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_DropContinuousQueryCommand_Command)
	v := ext.(*internal.DropContinuousQueryCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.DropContinuousQuery(v.GetDatabase(), v.GetName()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyCreateSubscriptionCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CreateSubscriptionCommand_Command)
	v := ext.(*internal.CreateSubscriptionCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.CreateSubscription(v.GetDatabase(), v.GetRetentionPolicy(), v.GetName(), v.GetMode(), v.GetDestinations()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyDropSubscriptionCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_DropSubscriptionCommand_Command)
	v := ext.(*internal.DropSubscriptionCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.DropSubscription(v.GetDatabase(), v.GetRetentionPolicy(), v.GetName()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyCreateUserCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CreateUserCommand_Command)
	v := ext.(*internal.CreateUserCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.CreateUser(v.GetName(), v.GetHash(), v.GetAdmin()); err != nil {
		return err
	}
	fsm.data = other

	return nil
}

func (fsm *storeFSM) applyDropUserCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_DropUserCommand_Command)
	v := ext.(*internal.DropUserCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.DropUser(v.GetName()); err != nil {
		return err
	}
	fsm.data = other
	return nil
}

func (fsm *storeFSM) applyUpdateUserCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_UpdateUserCommand_Command)
	v := ext.(*internal.UpdateUserCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.UpdateUser(v.GetName(), v.GetHash()); err != nil {
		return err
	}
	fsm.data = other
	return nil
}

func (fsm *storeFSM) applySetPrivilegeCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_SetPrivilegeCommand_Command)
	v := ext.(*internal.SetPrivilegeCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.SetPrivilege(v.GetUsername(), v.GetDatabase(), influxql.Privilege(v.GetPrivilege())); err != nil {
		return err
	}
	fsm.data = other
	return nil
}

func (fsm *storeFSM) applySetAdminPrivilegeCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_SetAdminPrivilegeCommand_Command)
	v := ext.(*internal.SetAdminPrivilegeCommand)

	// Copy data and update.
	other := fsm.data.Clone()
	if err := other.SetAdminPrivilege(v.GetUsername(), v.GetAdmin()); err != nil {
		return err
	}
	fsm.data = other
	return nil
}

func (fsm *storeFSM) applySetDataCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_SetDataCommand_Command)
	v := ext.(*internal.SetDataCommand)

	// Overwrite data.
	fsm.data = &Data{}
	fsm.data.unmarshal(v.GetData())

	return nil
}

func (fsm *storeFSM) applyCreateMetaNodeCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CreateMetaNodeCommand_Command)
	v := ext.(*internal.CreateMetaNodeCommand)

	other := fsm.data.Clone()
	other.CreateMetaNode(v.GetHTTPAddr(), v.GetTCPAddr())

	// If the cluster ID hasn't been set then use the command's random number.
	if other.ClusterID == 0 {
		other.ClusterID = v.GetRand()
	}

	fsm.data = other
	return nil
}

func (fsm *storeFSM) applySetMetaNodeCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_SetMetaNodeCommand_Command)
	v := ext.(*internal.SetMetaNodeCommand)

	other := fsm.data.Clone()
	other.SetMetaNode(v.GetHTTPAddr(), v.GetTCPAddr())

	// If the cluster ID hasn't been set then use the command's random number.
	if other.ClusterID == 0 {
		other.ClusterID = v.GetRand()
	}

	fsm.data = other
	return nil
}

func (fsm *storeFSM) applyDeleteMetaNodeCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_DeleteMetaNodeCommand_Command)
	v := ext.(*internal.DeleteMetaNodeCommand)

	other := fsm.data.Clone()
	node := other.MetaNode(v.GetID())
	if node == nil {
		return ErrNodeNotFound
	}

	if err := other.DeleteMetaNode(v.GetID()); err != nil {
		return err
	}
	fsm.data = other
	return nil
}

func (fsm *storeFSM) applyCreateDataNodeCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_CreateDataNodeCommand_Command)
	v := ext.(*internal.CreateDataNodeCommand)

	other := fsm.data.Clone()
	if err := other.CreateDataNode(v.GetHTTPAddr(), v.GetTCPAddr()); err != nil {
		return err
	}

	fsm.data = other
	return nil
}

func (fsm *storeFSM) applyDeleteDataNodeCommand(cmd *internal.Command) interface{} {
	ext, _ := proto.GetExtension(cmd, internal.E_DeleteDataNodeCommand_Command)
	v := ext.(*internal.DeleteDataNodeCommand)

	other := fsm.data.Clone()
	if err := other.DeleteDataNode(v.GetID()); err != nil {
		return err
	}
	fsm.data = other
	return nil
}

func (fsm *storeFSM) Snapshot() (raft.FSMSnapshot, error) {
	s := (*store)(fsm)
	s.mu.Lock()
	defer s.mu.Unlock()

	return &storeFSMSnapshot{Data: (*store)(fsm).data}, nil
}

func (fsm *storeFSM) Restore(r io.ReadCloser) error {
	// Read all bytes.
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	// Decode metadata.
	data := &Data{}
	if err := data.UnmarshalBinary(b); err != nil {
		return err
	}

	// Set metadata on store.
	// NOTE: No lock because Hashicorp Raft doesn't call Restore concurrently
	// with any other function.
	fsm.data = data

	return nil
}

type storeFSMSnapshot struct {
	Data *Data
}

func (s *storeFSMSnapshot) Persist(sink raft.SnapshotSink) error {
	err := func() error {
		// Encode data.
		p, err := s.Data.MarshalBinary()
		if err != nil {
			return err
		}

		// Write data to sink.
		if _, err := sink.Write(p); err != nil {
			return err
		}

		// Close the sink.
		if err := sink.Close(); err != nil {
			return err
		}

		return nil
	}()

	if err != nil {
		sink.Cancel()
		return err
	}

	return nil
}

// Release is invoked when we are finished with the snapshot
func (s *storeFSMSnapshot) Release() {}
