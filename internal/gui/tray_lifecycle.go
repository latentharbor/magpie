//go:build !nogui

package gui

import (
	"log"
	"runtime"
)

// Recover visibility persisted by older versions' update handovers.
func (h *host) restoreTray() {
	if runtime.GOOS == "darwin" {
		h.tray.Show()
	}
}

// The predecessor keeps its item alive for queued drawing callbacks, but
// takes no space in the bar and never persists visible=false.
func (h *host) retireTray() {
	if !trayRetire() {
		log.Println("update: couldn't retire the old menu bar item; it stays until this process exits")
	}
}
