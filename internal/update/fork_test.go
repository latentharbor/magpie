package update

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/testenv"
)

func TestForkOwnsUpdateFeeds(t *testing.T) {
	t.Setenv("MAGPIE_UPDATE_FEED", "")
	t.Setenv("MAGPIE_NOTES_FEED", "")
	home := "https://github.com/latentharbor/magpie/releases"
	if Feed() != home+"/latest/download/latest.json" || NotesFeed() != home+"/latest/download/notes.json" {
		t.Fatalf("feeds do not belong to this distribution: %s %s", Feed(), NotesFeed())
	}
	if ReleasePage("v0.1.1331") != home+"/tag/v0.1.1331" {
		t.Fatal("release notes link left this distribution")
	}
	t.Setenv("MAGPIE_UPDATE_FEED", "http://127.0.0.1:1/test-feed")
	t.Setenv("MAGPIE_NOTES_FEED", "http://127.0.0.1:1/test-notes")
	if Feed() != "http://127.0.0.1:1/test-feed" || NotesFeed() != "http://127.0.0.1:1/test-notes" {
		t.Fatal("explicit test feeds were overridden")
	}
}

func TestForkAppUpdateRequiresRealSameSigner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("codesign's fixture is a Unix shell script")
	}
	for _, tc := range []struct {
		name, installed, downloaded string
		broken, wantOK              bool
	}{
		{"same team", "TEAMONE", "TEAMONE", false, true},
		{"different team", "TEAMONE", "TEAMTWO", false, false},
		{"both ad hoc", "not set", "not set", false, false},
		{"installed ad hoc", "not set", "TEAMONE", false, false},
		{"downloaded ad hoc", "TEAMONE", "not set", false, false},
		{"signature invalid", "TEAMONE", "TEAMONE", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			exit := 0
			if tc.broken {
				exit = 1
			}
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --verify ]; then exit %d; fi\nif [ \"$2\" = installed.app ]; then echo TeamIdentifier=%s >&2; else echo TeamIdentifier=%s >&2; fi\n", exit, tc.installed, tc.downloaded)
			testenv.Program(t, filepath.Join(dir, "codesign"), script)
			t.Setenv("PATH", dir)
			err := sameSigner(context.Background(), "installed.app", "downloaded.app")
			if (err == nil) != tc.wantOK {
				t.Fatalf("signature accepted=%v, want %v: %v", err == nil, tc.wantOK, err)
			}
			if !tc.wantOK && err != nil && strings.Contains(tc.name, "ad hoc") && !strings.Contains(err.Error(), "Developer ID") {
				t.Fatalf("unsigned app did not explain manual bootstrap: %v", err)
			}
		})
	}
}
