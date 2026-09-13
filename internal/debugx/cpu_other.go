//go:build !unix

package debugx

// processCPUSeconds has no portable answer off Unix, so it reports nothing
// rather than an invented number.
//
// The consequence is visible and bounded: the live page's "cores busy" tile
// reads 0.00 and its lanes stay empty, because that figure is derived from the
// difference between two of these readings. Every other number on the page -
// goroutines, threads, heap, GC - comes from runtime/metrics and is unaffected.
// This file exists so the package compiles on Windows at all; the platform is
// not one anything here runs on.
func processCPUSeconds() float64 {
	return 0
}
