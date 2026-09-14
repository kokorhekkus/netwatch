//go:build !darwin || !cgo

package wifi

// Read reports no sample when CoreWLAN is unavailable.
//
// This keeps `CGO_ENABLED=0 go build` and non-Darwin builds working. Radio
// telemetry is the only part of netwatch that needs cgo, so it degrades to
// absent rather than failing the build; an Ethernet connection lands here too
// and has no radio to report.
func Read() Sample { return Sample{} }
