// Command buildconfig prepares a Go overlay for an embedded initial document.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: buildconfig <config-file> <command-directory> <output-directory>")
		os.Exit(2)
	}
	if err := writeOverlay(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "build initial config:", err)
		os.Exit(1)
	}
}

func writeOverlay(configPath, packageDir, outputDir string) error {
	pkg, err := filepath.Abs(packageDir)
	if err != nil {
		return fmt.Errorf("resolve command directory: %w", err)
	}
	out, err := filepath.Abs(outputDir)
	if err != nil {
		return fmt.Errorf("resolve build directory: %w", err)
	}
	placeholder := filepath.Join(pkg, "initial-config.base64")
	info, err := os.Stat(placeholder)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("command must provide a regular initial-config.base64 embed file")
	}
	pkgInfo, err := os.Stat(pkg)
	if err != nil {
		return fmt.Errorf("stat command directory: %w", err)
	}
	outInfo, err := os.Stat(out)
	if err != nil {
		return fmt.Errorf("stat build directory: %w", err)
	}
	if os.SameFile(pkgInfo, outInfo) {
		return fmt.Errorf("build output must not replace command source files")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read initial document: %w", err)
	}
	payload := filepath.Join(out, "initial-config.base64")
	overlayPath := filepath.Join(out, "overlay.json")
	overlay, err := json.Marshal(struct{ Replace map[string]string }{
		Replace: map[string]string{placeholder: payload},
	})
	if err != nil {
		return fmt.Errorf("encode build overlay: %w", err)
	}
	for _, destination := range []string{payload, overlayPath} {
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("build output already exists: %s", destination)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect build output: %w", err)
		}
	}
	if err := writeNewFile(payload, []byte(base64.StdEncoding.EncodeToString(data))); err != nil {
		return fmt.Errorf("write embedded document: %w", err)
	}
	if err := writeNewFile(overlayPath, overlay); err != nil {
		return fmt.Errorf("write build overlay: %w", err)
	}
	return nil
}

func writeNewFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create build file: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write build file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close build file: %w", err)
	}
	return nil
}
