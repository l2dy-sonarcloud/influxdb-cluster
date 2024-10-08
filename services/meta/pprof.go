package meta

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"net/http"
	httppprof "net/http/pprof"
	"path"
	"runtime/pprof"
	"runtime/trace"
	"strconv"
	"time"
)

// handleProfiles determines which profile to return to the requester.
func (h *handler) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/debug/pprof/cmdline":
		httppprof.Cmdline(w, r)
	case "/debug/pprof/profile":
		httppprof.Profile(w, r)
	case "/debug/pprof/symbol":
		httppprof.Symbol(w, r)
	case "/debug/pprof/all":
		h.archiveProfiles(w, r)
	default:
		httppprof.Index(w, r)
	}
}

// archiveProfiles collects the following profiles:
//   - goroutine profile
//   - heap profile
//   - blocking profile
//   - mutex profile
//   - (optionally) CPU profile
//
// All information is added to a tar archive and then compressed, before being
// returned to the requester as an archive file. Where profiles support debug
// parameters, the profile is collected with debug=1. To optionally include a
// CPU profile, the requester should provide a `cpu` query parameter, and can
// also provide a `seconds` parameter to specify a non-default profile
// collection time. The default CPU profile collection time is 30 seconds.
//
// Example request including CPU profile:
//
//	http://localhost:8086/debug/pprof/all?cpu=true&seconds=45
//
// The value after the `cpu` query parameter is not actually important, as long
// as there is something there.
func (h *handler) archiveProfiles(w http.ResponseWriter, r *http.Request) {
	// prof describes a profile name and a debug value, or in the case of a CPU
	// profile, the number of seconds to collect the profile for.
	type prof struct {
		Name     string        // name of profile
		Duration time.Duration // duration of profile if applicable.  curently only used by cpu and trace
	}

	var profiles = []prof{
		{Name: "goroutine"},
		{Name: "block"},
		{Name: "mutex"},
		{Name: "heap"},
		{Name: "allocs"},
		{Name: "threadcreate"},
	}

	// We parse the form here so that we can use the http.Request.Form map.
	//
	// Otherwise we'd have to use r.FormValue() which makes it impossible to
	// distinuish between a form value that exists and has no value and one that
	// does not exist at all.
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// In the following two blocks, we check if the request should include cpu
	// profiles and a trace log.
	//
	// Since the submitted form can contain multiple version of a variable like:
	//
	//   http://localhost:8086?cpu=1s&cpu=30s&trace=3s&cpu=5s
	//
	// the question arises: which value should we use?  We choose to use the LAST
	// value supplied.
	//
	// This is an edge case but if for some reason, for example, a url is
	// programatically built and multiple values are supplied, this will do what
	// is expected.
	//

	// last() returns either the last item from a slice of strings or an empty
	// string if the supplied slice is empty or nill.
	last := func(s []string) string {
		if len(s) == 0 {
			return ""
		}
		return s[len(s)-1]
	}

	// if trace exsits as a form value, add it to the profiles slice with the
	// decoded duration.
	//
	// Requests for a trace should look like:
	//
	//  ?trace=10s
	//
	if vals, exists := r.Form["trace"]; exists {
		// parse the duration encoded in the last "trace" value supplied.
		val := last(vals)
		duration, err := time.ParseDuration(val)

		// If we can't parse the duration or if the user supplies a negative
		// number, return an appropriate error status and message.
		//
		// In this case it is a StatusBadRequest (400) since the problem is in the
		// supplied form data.
		if duration < 0 {
			http.Error(w, fmt.Sprintf("negative trace durations not allowed"), http.StatusBadRequest)
			return
		}

		if err != nil {
			http.Error(w, fmt.Sprintf("could not parse supplied duration for trace %q", val), http.StatusBadRequest)
			return
		}

		// Trace files can get big.  Lets clamp the maximum trace duration to 45s.
		if maxDuration := time.Second * 45; duration > maxDuration {
			duration = maxDuration
		}
		profiles = append(profiles, prof{"trace", duration})
	}

	// Capturing CPU profiles is a little tricker.  The preferred way to send the
	// the cpu profile duration is via the supplied "cpu" variable's value.
	//
	// The duration should be encoded as a Go duration that can be parsed by
	// time.ParseDuration().
	//
	// In the past users were encouraged to assign any value to cpu and provide
	// the duration in a separate "seconds" value.
	//
	// The code below handles both -- first it attempts to use the old method
	// which would look like:
	//
	//    ?cpu=foobar&seconds=10
	//
	// Then it attempts to ascertain the duration provided with:
	//
	//    ?cpu=10s
	//
	// This preserves backwards compatibility with any tools that have been
	// written to gather profiles.
	//
	if vals, exists := r.Form["cpu"]; exists {
		duration := time.Second * 30
		val := last(vals)

		// getDuration is a small function literal that encapsulates the logic
		// for getting the duration from either the "seconds" form value or from
		// the value assigned to "cpu".
		getDuration := func() (time.Duration, error) {
			if seconds, exists := r.Form["seconds"]; exists {
				s, err := strconv.ParseInt(last(seconds), 10, 64)
				if err != nil {
					return 0, err
				}
				return time.Second * time.Duration(s), nil
			}
			// see if the value of cpu is a duration like:  cpu=10s
			return time.ParseDuration(val)
		}

		duration, err := getDuration()
		if err != nil {
			http.Error(w, fmt.Sprintf("could not parse supplied duration for cpu profile %q", val), http.StatusBadRequest)
			return
		}

		if duration < 0 {
			http.Error(w, fmt.Sprintf("negative cpu profile durations not allowed"), http.StatusBadRequest)
			return
		}

		// prepend our profiles slice with cpu -- we want to fetch cpu profiles
		// first.
		profiles = append([]prof{{"cpu", duration}}, profiles...)
	}

	tarball := &bytes.Buffer{}
	buf := &bytes.Buffer{} // Temporary buffer for each profile result.

	tw := tar.NewWriter(tarball)
	// Collect and write out profiles.
	for _, profile := range profiles {
		switch profile.Name {
		case "cpu":
			if err := pprof.StartCPUProfile(buf); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			sleep(r, profile.Duration)
			pprof.StopCPUProfile()

		case "trace":
			if err := trace.Start(buf); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			sleep(r, profile.Duration)
			trace.Stop()

		default:
			prof := pprof.Lookup(profile.Name)
			if prof == nil {
				http.Error(w, "unable to find profile "+profile.Name, http.StatusInternalServerError)
				return
			}

			if err := prof.WriteTo(buf, 0); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// Write the profile file's header.
		err := tw.WriteHeader(&tar.Header{
			Name: path.Join("profiles", profile.Name+".pb.gz"),
			Mode: 0600,
			Size: int64(buf.Len()),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		// Write the profile file's data.
		if _, err := tw.Write(buf.Bytes()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		// Reset the buffer for the next profile.
		buf.Reset()
	}

	// Close the tar writer.
	if err := tw.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return the gzipped archive.
	w.Header().Set("Content-Disposition", "attachment; filename=profiles.tar")
	w.Header().Set("Content-Type", "application/x-tar")
	io.Copy(w, tarball)
}

// Taken from net/http/pprof/pprof.go
func sleep(r *http.Request, d time.Duration) {
	// wait for either the timer to expire or the contex
	select {
	case <-time.After(d):
	case <-r.Context().Done():
	}
}
