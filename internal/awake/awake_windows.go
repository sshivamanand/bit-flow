package awake

import (
	"runtime"
	"syscall"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var setThreadExecutionState = syscall.NewLazyDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// Hold prevents idle sleep until release is called. The execution state is
// per thread, so a dedicated locked OS thread holds it.
func Hold() (release func()) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		setThreadExecutionState.Call(esContinuous | esSystemRequired)
		<-stop
		setThreadExecutionState.Call(esContinuous)
	}()
	return func() { close(stop); <-done }
}
