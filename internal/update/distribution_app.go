package update

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/yetone/magpie/internal/proc"
)

// unpackDistribution accepts only the stock, symlink-free app tree. Extract
// through Go's ZIP reader too: validating central directory names before
// handing an archive to another extractor leaves parser differences in scope.
// ditto's AppleDouble metadata is harmless here and deliberately not restored;
// stock builds need no resource forks or extended attributes to verify.
func unpackDistribution(archive, out string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	seen := map[string]bool{}
	required := map[string]bool{"magpie.app/Contents/Info.plist": false, "magpie.app/Contents/MacOS/magpie": false}
	var total uint64
	for _, f := range r.File {
		name := strings.TrimSuffix(f.Name, "/")
		if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") ||
			strings.ContainsAny(name, "\\:") || seen[strings.ToLower(name)] {
			return errors.New("distribution archive contains an ambiguous path")
		}
		// The generated stock bundle has ASCII names. This also avoids Unicode
		// normalization aliases on case-insensitive macOS volumes.
		for _, ch := range name {
			if ch < 32 || ch > 126 {
				return errors.New("distribution archive contains a non-stock path")
			}
		}
		seen[strings.ToLower(name)] = true
		mode := f.Mode()
		if (!mode.IsRegular() && !mode.IsDir()) || mode.IsDir() != strings.HasSuffix(f.Name, "/") || f.Flags&1 != 0 {
			return errors.New("distribution archive contains a link, special file or encrypted entry")
		}
		app := name == "magpie.app" || strings.HasPrefix(name, "magpie.app/")
		metadata := name == "__MACOSX" && mode.IsDir() ||
			name == "__MACOSX/._magpie.app" && mode.IsRegular() ||
			(name == "__MACOSX/magpie.app" || strings.HasPrefix(name, "__MACOSX/magpie.app/")) &&
				(mode.IsDir() || strings.HasPrefix(path.Base(name), "._"))
		if !app && !metadata || name == "magpie.app" && !mode.IsDir() {
			return errors.New("distribution archive contains a file outside magpie.app")
		}
		if f.UncompressedSize64 > 2<<30 || total > (2<<30)-f.UncompressedSize64 {
			return errors.New("distribution archive expands beyond its size limit")
		}
		total += f.UncompressedSize64
		if _, ok := required[name]; ok {
			if !mode.IsRegular() || name == "magpie.app/Contents/MacOS/magpie" && mode.Perm()&0o111 == 0 {
				return errors.New("distribution archive is missing a regular app executable or plist")
			}
			required[name] = true
		}
	}
	for _, present := range required {
		if !present {
			return errors.New("distribution archive is missing the stock app executable or plist")
		}
	}
	for _, f := range r.File {
		// Without --sequesterRsrc ditto puts AppleDouble ._* sidecars
		// directly beside the app's files rather than under __MACOSX.
		// Restoring them as real files breaks the code resource seal.
		if strings.HasPrefix(f.Name, "__MACOSX/") || f.Name == "__MACOSX/" ||
			f.Mode().IsRegular() && strings.HasPrefix(path.Base(f.Name), "._") {
			continue
		}
		dest := filepath.Join(out, filepath.FromSlash(f.Name))
		if f.Mode().IsDir() {
			err = os.MkdirAll(dest, 0o755)
		} else {
			err = unpackDistributionFile(f, dest)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func unpackDistributionFile(f *zip.File, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := f.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.Mode().Perm()&0o777)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, int64(f.UncompressedSize64)+1))
	closed := out.Close()
	if err != nil {
		return err
	}
	if uint64(n) != f.UncompressedSize64 {
		return errors.New("distribution archive entry has the wrong expanded size")
	}
	return closed
}

// An authenticated archive must still identify the app and version that the
// manifest promised. This applies before either the Apple or ad-hoc signer path.
func distributionBundle(ctx context.Context, app, version string) error {
	for _, field := range []struct{ key, value string }{
		{"CFBundleIdentifier", "com.yetone.magpie"},
		{"CFBundleExecutable", "magpie"},
		{"CFBundlePackageType", "APPL"},
		{"CFBundleShortVersionString", version},
		{"CFBundleVersion", version},
	} {
		b, err := proc.CommandContext(ctx, "plutil", "-extract", field.key, "raw", "-o", "-", filepath.Join(app, "Contents", "Info.plist")).Output()
		if err != nil || strings.TrimSuffix(string(b), "\n") != field.value {
			return fmt.Errorf("distribution app has an invalid %s", field.key)
		}
	}
	return nil
}
