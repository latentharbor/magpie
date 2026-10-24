//go:build darwin && cgo && !nogui

package gui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

func TestUpdateHandoverDoesNotPersistHiddenTray(t *testing.T) {
	for _, mode := range []string{"restore", "retire"} {
		t.Run(mode, func(t *testing.T) {
			out, err := runAppKit(t, 30*time.Second, []string{"MAGPIE_TEST_TRAY_LIFECYCLE=" + mode})
			if err != nil || !strings.Contains(string(out), "tray lifecycle: ok") {
				t.Fatalf("tray lifecycle: %v\n%s", err, out)
			}
		})
	}
}

func init() {
	mode := os.Getenv("MAGPIE_TEST_TRAY_LIFECYCLE")
	if mode != "restore" && mode != "retire" {
		return
	}
	app := application.New(application.Options{
		Name: "magpie tray lifecycle regression",
		Mac:  application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
	})
	h := &host{app: app}
	h.tray = app.SystemTray.New()
	h.tray.SetTemplateIcon(trayIcon)
	fail := func(format string, a ...any) {
		fmt.Printf("tray lifecycle: "+format+"\n", a...)
		os.Exit(1)
	}
	state := func(wantVisible bool, wantZero bool) {
		// Hide and Show dispatch another block even when called on the
		// application thread. Wait for that block, not a fixed sleep.
		for deadline := time.Now().Add(5 * time.Second); ; {
			visible, length, ok := trayNativeState()
			if ok && visible == wantVisible && (length == 0) == wantZero {
				return
			}
			if time.Now().After(deadline) {
				fail("visible=%v length=%v found=%v, want visible=%v zero=%v", visible, length, ok, wantVisible, wantZero)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go func() {
			state(true, false)
			if mode == "restore" {
				// An older version left this persisted visibility behind.
				h.tray.Hide()
				state(false, false)
				h.restoreTray()
				state(true, false)
			}
			h.retireTray()
			state(true, true)
			// Updates already queued by the predecessor's quota watcher
			// must not put its icon back or dereference a destroyed item.
			h.tray.SetLabel("99%")
			h.tray.SetTemplateIcon(trayIcon)
			h.tray.SetTooltip("draining")
			application.InvokeSync(func() {})
			state(true, true)
			h.retireTray()
			state(true, true)
			fmt.Println("tray lifecycle: ok")
			app.Quit()
		}()
	})
	if err := app.Run(); err != nil {
		fail("run: %v", err)
	}
	os.Exit(0)
}
