package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runCLI(ctx, os.Args[1:], os.Stdout, execRunner{}); err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "release: %v\n", err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func runCLI(
	ctx context.Context,
	args []string,
	output io.Writer,
	runner commandRunner,
) error {
	if len(args) == 0 {
		return usageError()
	}

	switch args[0] {
	case "source":
		flags := flag.NewFlagSet("source", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		repoFlag := flags.String("repo", "", "repository root")
		if err := parseCommandFlags(flags, args[1:]); err != nil {
			return err
		}
		repo, manifest, err := commandContext(*repoFlag)
		if err != nil {
			return err
		}
		return runSourcePreflight(repo, manifest, output)

	case "list":
		flags := flag.NewFlagSet("list", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		repoFlag := flags.String("repo", "", "repository root")
		layer := flags.Int("layer", -1, "release layer, or -1 for every layer")
		format := flags.String("format", "path", "path, import, tag, or tsv")
		version := flags.String("version", "", "stable release version (required for tag format)")
		if err := parseCommandFlags(flags, args[1:]); err != nil {
			return err
		}
		_, manifest, err := commandContext(*repoFlag)
		if err != nil {
			return err
		}
		return listModules(manifest, *layer, *format, *version, output)

	case "strict-all":
		flags := flag.NewFlagSet("strict-all", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		repoFlag := flags.String("repo", "", "repository root")
		version := flags.String("version", "", "stable release version")
		if err := parseCommandFlags(flags, args[1:]); err != nil {
			return err
		}
		repo, manifest, err := commandContext(*repoFlag)
		if err != nil {
			return err
		}
		if err := requireFlag("version", *version); err != nil {
			return err
		}
		if err := strictAll(ctx, runner, repo, manifest, *version); err != nil {
			return err
		}
		return writeOutput(output, "Strict published-module gate PASS for %s.\n", *version)

	case "strict-tag":
		flags := flag.NewFlagSet("strict-tag", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		repoFlag := flags.String("repo", "", "repository root")
		tag := flags.String("tag", "", "pushed module tag")
		if err := parseCommandFlags(flags, args[1:]); err != nil {
			return err
		}
		repo, manifest, err := commandContext(*repoFlag)
		if err != nil {
			return err
		}
		if err := requireFlag("tag", *tag); err != nil {
			return err
		}
		entry, version, err := manifest.moduleForTag(*tag)
		if err != nil {
			return err
		}
		if err := strictModule(ctx, runner, repo, manifest, entry.Path, version, true); err != nil {
			return err
		}
		commit, err := tagCommitAtHead(ctx, runner, repo, *tag)
		if err != nil {
			return err
		}
		return printModuleOutputs(output, manifest, entry, version, *tag, commit)

	case "strict-module":
		flags := flag.NewFlagSet("strict-module", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		repoFlag := flags.String("repo", "", "repository root")
		modulePath := flags.String("module", "", "published module path")
		version := flags.String("version", "", "stable release version")
		if err := parseCommandFlags(flags, args[1:]); err != nil {
			return err
		}
		repo, manifest, err := commandContext(*repoFlag)
		if err != nil {
			return err
		}
		if err := requireFlags(map[string]string{"module": *modulePath, "version": *version}); err != nil {
			return err
		}
		if err := strictModule(ctx, runner, repo, manifest, *modulePath, *version, false); err != nil {
			return err
		}
		return writeOutput(output, "Strict pre-tag gate PASS for %s at %s.\n", *modulePath, *version)

	case "stage-module":
		flags := flag.NewFlagSet("stage-module", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		repoFlag := flags.String("repo", "", "repository root")
		modulePath := flags.String("module", "", "published module path")
		version := flags.String("version", "", "stable release version")
		if err := parseCommandFlags(flags, args[1:]); err != nil {
			return err
		}
		repo, manifest, err := commandContext(*repoFlag)
		if err != nil {
			return err
		}
		if err := requireFlags(map[string]string{"module": *modulePath, "version": *version}); err != nil {
			return err
		}
		if err := stagePublishedModule(
			ctx,
			runner,
			repo,
			manifest,
			*modulePath,
			*version,
		); err != nil {
			return err
		}
		return writeOutput(
			output,
			"Staged and strictly verified %s at %s; review and commit go.mod/go.sum.\n",
			*modulePath,
			*version,
		)

	case "smoke":
		flags := flag.NewFlagSet("smoke", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		repoFlag := flags.String("repo", "", "repository root")
		tag := flags.String("tag", "", "stable cmd/gobridge tag")
		if err := parseCommandFlags(flags, args[1:]); err != nil {
			return err
		}
		repo, manifest, err := commandContext(*repoFlag)
		if err != nil {
			return err
		}
		if err := requireFlag("tag", *tag); err != nil {
			return err
		}
		if err := runConsumerSmoke(ctx, runner, repo, manifest, *tag); err != nil {
			return err
		}
		return writeOutput(output, "External consumer smoke PASS for %s.\n", *tag)

	case "remote-tag":
		flags := flag.NewFlagSet("remote-tag", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		repoFlag := flags.String("repo", "", "repository root")
		remote := flags.String("remote", "origin", "Git remote")
		tag := flags.String("tag", "", "validated release tag")
		commit := flags.String("commit", "", "validated release tag commit")
		if err := parseCommandFlags(flags, args[1:]); err != nil {
			return err
		}
		repo, manifest, err := commandContext(*repoFlag)
		if err != nil {
			return err
		}
		if err := requireFlags(map[string]string{
			"commit": *commit,
			"remote": *remote,
			"tag":    *tag,
		}); err != nil {
			return err
		}
		if _, _, err := manifest.moduleForTag(*tag); err != nil {
			return err
		}
		if err := verifyRemoteTagCommit(
			ctx,
			runner,
			repo,
			*remote,
			*tag,
			*commit,
		); err != nil {
			return err
		}
		return writeOutput(output, "Remote tag %s matches %s.\n", *tag, *commit)

	default:
		return usageError()
	}
}

func commandContext(repoFlag string) (string, releaseManifest, error) {
	repo, err := findRepository(repoFlag)
	if err != nil {
		return "", releaseManifest{}, err
	}
	manifest, err := loadManifest(repo)
	if err != nil {
		return "", releaseManifest{}, err
	}
	return repo, manifest, nil
}

func findRepository(repoFlag string) (string, error) {
	if repoFlag != "" {
		repo, err := filepath.Abs(repoFlag)
		if err != nil {
			return "", fmt.Errorf("resolving repository path %s: %w", repoFlag, err)
		}
		repo, err = filepath.EvalSymlinks(repo)
		if err != nil {
			return "", fmt.Errorf("resolving repository symlinks %s: %w", repoFlag, err)
		}
		manifestPath, err := secureJoin(repo, manifestRelativePath)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", manifestRelativePath, err)
		}
		if _, err := os.Stat(manifestPath); err != nil {
			return "", fmt.Errorf("repository %s has no %s: %w", repo, manifestRelativePath, err)
		}
		return repo, nil
	}

	current, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting current directory: %w", err)
	}
	for {
		if manifestPath, pathErr := secureJoin(current, manifestRelativePath); pathErr == nil {
			if _, statErr := os.Stat(manifestPath); statErr == nil {
				resolved, err := filepath.EvalSymlinks(current)
				if err != nil {
					return "", fmt.Errorf("resolving repository symlinks %s: %w", current, err)
				}
				return resolved, nil
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("could not find %s above current directory", manifestRelativePath)
		}
		current = parent
	}
}

func runSourcePreflight(repo string, manifest releaseManifest, output io.Writer) error {
	state, err := inspectRepository(repo, manifest, "")
	if err != nil {
		return err
	}

	layerCounts := make(map[int]int)
	for _, entry := range manifest.Published {
		layerCounts[entry.Layer]++
	}
	layers := make([]int, 0, len(layerCounts))
	for layer := range layerCounts {
		layers = append(layers, layer)
	}
	slices.Sort(layers)

	if err := writeOutput(output, "Release source preflight PASS.\n"); err != nil {
		return err
	}
	if err := writeOutput(output, "Published modules: %d", len(manifest.Published)); err != nil {
		return err
	}
	for _, layer := range layers {
		if err := writeOutput(output, "; layer %d=%d", layer, layerCounts[layer]); err != nil {
			return err
		}
	}
	if err := writeOutput(output, "\n"); err != nil {
		return err
	}
	if err := printMigrationInventory(output, "Published manifest migration inventory", state.Violations); err != nil {
		return err
	}
	if len(state.Violations) != 0 {
		return writeOutput(
			output,
			"Source preflight permits the reported migration inventory; strict release gates reject it.\n",
		)
	}
	return nil
}

func printMigrationInventory(
	output io.Writer,
	heading string,
	violations []manifestViolation,
) error {
	counts := make(map[violationKind]int)
	for _, violation := range violations {
		counts[violation.Kind]++
	}
	if err := writeOutput(output, "%s: %d\n", heading, len(violations)); err != nil {
		return err
	}
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, string(kind))
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		if err := writeOutput(output, "  %s: %d\n", kind, counts[violationKind(kind)]); err != nil {
			return err
		}
	}
	for _, violation := range violations {
		if err := writeOutput(output, "  - %s\n", violation.Error()); err != nil {
			return err
		}
	}
	return nil
}

func listModules(
	manifest releaseManifest,
	layer int,
	format string,
	version string,
	output io.Writer,
) error {
	if layer < -1 {
		return fmt.Errorf("layer must be -1 or non-negative, got %d", layer)
	}
	if format == "tag" {
		if err := validateStableVersion(version); err != nil {
			return err
		}
	} else if version != "" {
		return errors.New("--version is only valid with --format=tag")
	}
	if !slices.Contains([]string{"path", "import", "tag", "tsv"}, format) {
		return fmt.Errorf("format %q is not one of path, import, tag, tsv", format)
	}

	foundLayer := false
	for _, entry := range manifest.Published {
		if layer >= 0 && entry.Layer != layer {
			continue
		}
		foundLayer = true
		switch format {
		case "path":
			if err := writeOutput(output, "%s\n", entry.Path); err != nil {
				return err
			}
		case "import":
			if err := writeOutput(output, "%s\n", manifest.importPath(entry.Path)); err != nil {
				return err
			}
		case "tag":
			if err := writeOutput(output, "%s\n", tagFor(entry.Path, version)); err != nil {
				return err
			}
		case "tsv":
			if err := writeOutput(
				output,
				"%d\t%s\t%s\n",
				entry.Layer,
				entry.Path,
				manifest.importPath(entry.Path),
			); err != nil {
				return err
			}
		}
	}
	if layer >= 0 && !foundLayer {
		return fmt.Errorf("release layer %d is not declared", layer)
	}
	return nil
}

func printModuleOutputs(
	output io.Writer,
	manifest releaseManifest,
	entry publishedModule,
	version string,
	tag string,
	commit string,
) error {
	// GitHub Actions consumes these key/value lines through GITHUB_OUTPUT.
	return writeOutput(
		output,
		"tag=%s\nversion=%s\npath=%s\nimport=%s\nlayer=%d\ncommit=%s\n",
		tag,
		version,
		entry.Path,
		manifest.importPath(entry.Path),
		entry.Layer,
		commit,
	)
}

func parseCommandFlags(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parsing %s flags: %w", flags.Name(), err)
	}
	return nil
}

func writeOutput(output io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		return fmt.Errorf("writing release command output: %w", err)
	}
	return nil
}

func requireFlag(name, value string) error {
	if value == "" {
		return fmt.Errorf("--%s is required", name)
	}
	return nil
}

func requireFlags(values map[string]string) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := requireFlag(name, values[name]); err != nil {
			return err
		}
	}
	return nil
}

func usageError() error {
	return errors.New(
		"usage: release <source|list|strict-all|strict-tag|strict-module|" +
			"stage-module|smoke|remote-tag> [flags]",
	)
}
