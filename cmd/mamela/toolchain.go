package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ieyoukan/mamela/internal/evaluate"
	"github.com/ieyoukan/mamela/internal/toolchain"
	"github.com/ieyoukan/mamela/internal/version"
)

// toolFlags are the flags that choose the toolchain and the Pome cache. Empty means: find them.
type toolFlags struct {
	dir, pomeCache *string
}

func addToolFlags(fs *flag.FlagSet) toolFlags {
	return toolFlags{
		dir: fs.String("toolchain", "", "directory holding the pinned emela and wasmtime "+
			"(default: $MAMELA_TOOLCHAIN_DIR, else .tools/bin in this directory or above, else `mamela toolchain install`'s)"),
		pomeCache: fs.String("pome-cache", "", "directory of already fetched Pome dependencies; builds never fetch "+
			"(default: $EMELA_POME_CACHE, else .tools/pomes next to the repository toolchain, else ~/.emela/pomes)"),
	}
}

// tools is the chosen toolchain and Pome cache, and how they were chosen.
type tools struct {
	toolchain evaluate.Toolchain
	dir       string
	source    string
	pomeCache string
}

func (f toolFlags) find() (tools, error) {
	l := toolchain.Pinned()
	var loc toolchain.Location
	switch {
	case *f.dir != "":
		loc = toolchain.Location{Dir: *f.dir, Source: "--toolchain"}
	case os.Getenv("MAMELA_TOOLCHAIN_DIR") != "":
		loc = toolchain.Location{Dir: os.Getenv("MAMELA_TOOLCHAIN_DIR"), Source: "MAMELA_TOOLCHAIN_DIR"}
	default:
		cwd, err := os.Getwd()
		if err != nil {
			return tools{}, err
		}
		if loc, err = l.Locate(cwd, exec.LookPath); errors.Is(err, toolchain.ErrNotFound) {
			return tools{}, &evaluate.Error{Code: evaluate.CodeToolchain, Stage: evaluate.StageToolchain,
				Message: fmt.Sprintf("the pinned toolchain (emela %s, wasmtime %s) is not installed: there is no .tools/bin "+
					"in this directory or above it, none on PATH, and no user installation", l.Emela.Version, l.Wasmtime.Version),
				Hint: "run `mamela toolchain install` (in the Mamela repository: `mise install` or scripts/install-toolchain.sh)"}
		} else if err != nil {
			return tools{}, err
		}
	}
	t := tools{dir: loc.Dir, source: loc.Source}
	t.pomeCache = *f.pomeCache
	if t.pomeCache == "" {
		t.pomeCache = os.Getenv("EMELA_POME_CACHE")
	}
	if t.pomeCache == "" {
		t.pomeCache = loc.PomeCache
	}
	if t.pomeCache == "" {
		t.pomeCache = defaultPomeCache()
	}
	if loc.Dir == "" {
		t.dir = filepath.Dir(loc.Emela)
		t.toolchain = evaluate.Toolchain{Emela: loc.Emela, Wasmtime: loc.Wasmtime}
		return t, nil
	}
	tc, err := evaluate.ToolchainInDir(loc.Dir)
	t.toolchain = tc
	return t, err
}

func (t tools) evaluator() *evaluate.Evaluator {
	return &evaluate.Evaluator{Toolchain: t.toolchain, Limits: evaluate.DefaultLimits, PomeCache: t.pomeCache}
}

const toolchainUsage = `Usage:
  mamela toolchain [status]   show which toolchain and Pome cache mamela uses, and check them
  mamela toolchain install    download and install the pinned emela and wasmtime (sha256-checked)

Mamela evaluates declarations only with the exact emela and wasmtime of toolchain.lock.
`

func runToolchain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	sub := "status"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "status":
		return runToolchainStatus(ctx, args, stdout, stderr)
	case "install":
		return runToolchainInstall(ctx, args, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, toolchainUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "mamela toolchain: unknown command %q\n\n%s", sub, toolchainUsage)
		return 2
	}
}

func runToolchainStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela toolchain status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := addToolFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, toolchainUsage)
		return 2
	}
	l := toolchain.Pinned()
	v := version.Current()
	if v == "unknown" {
		v += " (built without version information, as by go run)"
	}
	fmt.Fprintf(stdout, "mamela      %s\nplatform    %s\npinned      emela %s, wasmtime %s (toolchain.lock)\n",
		v, toolchain.Platform(), l.Emela.Version, l.Wasmtime.Version)
	t, err := tf.find()
	if t.dir != "" {
		if filepath.Dir(t.toolchain.Wasmtime) != t.dir && t.toolchain.Wasmtime != "" {
			fmt.Fprintf(stdout, "toolchain   %s\n            %s (%s)\n", t.toolchain.Emela, t.toolchain.Wasmtime, t.source)
		} else {
			fmt.Fprintf(stdout, "toolchain   %s (%s)\n", t.dir, t.source)
		}
	}
	if err == nil {
		_, err = t.toolchain.Verify(ctx)
	}
	if err != nil {
		fmt.Fprintln(stdout)
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "            ok: the pinned executables\npome cache  %s\n", t.pomeCache)
	return 0
}

func runToolchainInstall(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mamela toolchain install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	l := toolchain.Pinned()
	userBin, userDownloads, userErr := l.UserDirs()
	dir := fs.String("dir", userBin, "where to install emela and wasmtime")
	downloads := fs.String("downloads", userDownloads, "cache of the downloaded archives")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: mamela toolchain install [flags]\n\n"+
			"Downloads the pinned emela and wasmtime (toolchain.lock), checks the sha256 of each archive and\n"+
			"executable, and installs them. Mamela finds them there by itself.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if *dir == "" || *downloads == "" {
		fmt.Fprintf(stderr, "mamela toolchain install: %v\n", userErr)
		return 1
	}
	o := toolchain.Options{Dir: *dir, Downloads: *downloads, Log: stdout, LookPath: exec.LookPath}
	if err := l.Install(ctx, o); err != nil {
		fmt.Fprintf(stderr, "mamela toolchain install: %v\n", err)
		return 1
	}
	abs, _ := filepath.Abs(*dir)
	fmt.Fprintf(stdout, "\nthe pinned toolchain is in %s\n", abs)
	if *dir == userBin {
		fmt.Fprintln(stdout, "mamela finds it there by itself.")
	} else {
		fmt.Fprintf(stdout, "mamela finds it with --toolchain %s or MAMELA_TOOLCHAIN_DIR (or as .tools/bin of a directory above).\n", abs)
	}
	fmt.Fprintf(stdout, "To run emela yourself (for example `emela pome add`): export PATH=\"%s:$PATH\"\n", abs)
	return 0
}

func runVersion(stdout io.Writer) int {
	l := toolchain.Pinned()
	fmt.Fprintf(stdout, "mamela %s (toolchain: emela %s, wasmtime %s)\n", version.Current(), l.Emela.Version, l.Wasmtime.Version)
	return 0
}
