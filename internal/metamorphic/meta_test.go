// Copyright 2019 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package metamorphic

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/errorfs"
	"github.com/cockroachdb/pebble/internal/randvar"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/rand"
)

// TODO(peter):
//
// Miscellaneous:
// - Add support for different comparers. In particular, allow reverse
//   comparers and a comparer which supports Comparer.Split (by splitting off
//   a variable length suffix).
// - DeleteRange can be used to replace Delete, stressing the DeleteRange
//   implementation.
// - Add support for Writer.LogData

var (
	dir = flag.String("dir", "_meta",
		"the directory storing test state")
	disk = flag.Bool("disk", false,
		"whether to use an in-mem DB or on-disk (in-mem is significantly faster)")
	// TODO: default error rate to a non-zero value
	errorRate = flag.Float64("error-rate", 0.0,
		"rate of errors injected into filesystem operations (0 ≤ r < 1)")
	failRE = flag.String("fail", "",
		"fail the test if the supplied regular expression matches the output")
	traceFile = flag.String("trace-file", "",
		"write an execution trace to `<run-dir>/file`")
	keep = flag.Bool("keep", false,
		"keep the DB directory even on successful runs")
	ops    = randvar.NewFlag("uniform:5000-10000")
	runDir = flag.String("run-dir", "",
		"the specific configuration to (re-)run (used for post-mortem debugging)")
)

func init() {
	flag.Var(ops, "ops", "")
}

func testMetaRun(t *testing.T, runDir string) {
	opsPath := filepath.Join(filepath.Dir(filepath.Clean(runDir)), "ops")
	opsData, err := ioutil.ReadFile(opsPath)
	require.NoError(t, err)

	ops, err := parse(opsData)
	require.NoError(t, err)
	_ = ops

	optionsPath := filepath.Join(runDir, "OPTIONS")
	optionsData, err := ioutil.ReadFile(optionsPath)
	require.NoError(t, err)

	opts := &pebble.Options{}
	testOpts := &testOptions{opts: opts}
	require.NoError(t, parseOptions(testOpts, string(optionsData)))

	// Always use our custom comparer which provides a Split method.
	opts.Comparer = &comparer
	// Use an archive cleaner to ease post-mortem debugging.
	opts.Cleaner = base.ArchiveCleaner{}

	// Set up the filesystem to use for the test. Note that by default we use an
	// in-memory FS.
	if *disk && !testOpts.strictFS {
		opts.FS = vfs.Default
		require.NoError(t, os.RemoveAll(opts.FS.PathJoin(runDir, "data")))
	} else {
		opts.Cleaner = base.ArchiveCleaner{}
		if testOpts.strictFS {
			opts.FS = vfs.NewStrictMem()
		} else {
			opts.FS = vfs.NewMem()
		}
	}
	// Wrap the filesystem with one that will inject errors into read
	// operations with *errorRate probability.
	opts.FS = errorfs.Wrap(opts.FS, errorfs.WithProbability(errorfs.OpRead, *errorRate))

	if opts.WALDir != "" {
		opts.WALDir = opts.FS.PathJoin(runDir, opts.WALDir)
	}

	historyPath := filepath.Join(runDir, "history")
	historyFile, err := os.Create(historyPath)
	require.NoError(t, err)
	defer historyFile.Close()

	writers := []io.Writer{historyFile}
	if testing.Verbose() {
		writers = append(writers, os.Stdout)
	}
	h := newHistory(*failRE, writers...)

	m := newTest(ops)
	require.NoError(t, m.init(h, opts.FS.PathJoin(runDir, "data"), testOpts))
	for m.step(h) {
		if h.Failed() {
			if len(*failRE) > 0 {
				fmt.Fprintf(os.Stderr, "failure regex %q matched\n", *failRE)
			}
			m.maybeSaveData()
			os.Exit(1)
		}
	}

	if *keep && !*disk {
		m.maybeSaveData()
	}
}

// TestMeta generates a random set of operations to run, then runs the test
// with different options. See standardOptions() for the set of options that
// are always run, and randomOptions() for the randomly generated options. The
// number of operations to generate is determined by the `--ops` flag. If a
// failure occurs, the output is kept in `_meta/<test>`, though note that a
// subsequent invocation will overwrite that output. A test can be re-run by
// using the `--run-dir` flag. For example:
//
//   go test -v -run TestMeta --run-dir _meta/standard-017
//
// This will reuse the existing operations present in _meta/ops, rather than
// generating a new set.
func TestMeta(t *testing.T) {
	if *runDir != "" {
		// The --run-dir flag is specified either in the child process (see
		// runOptions() below) or the user specified it manually in order to re-run
		// a test.
		testMetaRun(t, *runDir)
		return
	}

	rootName := t.Name()

	// Cleanup any previous state.
	metaDir := filepath.Join(*dir, time.Now().Format("060102-150405.000"))
	require.NoError(t, os.RemoveAll(metaDir))
	require.NoError(t, os.MkdirAll(metaDir, 0755))
	defer func() {
		if !t.Failed() && !*keep {
			_ = os.RemoveAll(metaDir)
		}
	}()

	// Generate a new set of random ops, writing them to <dir>/ops. These will be
	// read by the child processes when performing a test run.
	ops := generate(ops.Uint64(), defaultConfig)
	opsPath := filepath.Join(metaDir, "ops")
	formattedOps := formatOps(ops)
	require.NoError(t, ioutil.WriteFile(opsPath, []byte(formattedOps), 0644))

	// Perform a particular test run with the specified options. The options are
	// written to <run-dir>/OPTIONS and a child process is created to actually
	// execute the test.
	runOptions := func(t *testing.T, opts *testOptions) {
		if opts.opts.Cache != nil {
			defer opts.opts.Cache.Unref()
		}

		runDir := filepath.Join(metaDir, path.Base(t.Name()))
		require.NoError(t, os.MkdirAll(runDir, 0755))

		optionsPath := filepath.Join(runDir, "OPTIONS")
		optionsStr := optionsToString(opts)
		require.NoError(t, ioutil.WriteFile(optionsPath, []byte(optionsStr), 0644))

		args := []string{
			"-disk=" + fmt.Sprint(*disk),
			"-keep=" + fmt.Sprint(*keep),
			"-run-dir=" + runDir,
			"-test.run=" + rootName + "$",
		}
		if *traceFile != "" {
			args = append(args, "-test.trace="+filepath.Join(runDir, *traceFile))
		}

		cmd := exec.Command(os.Args[0], args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf(`
===== ERR =====
%v
===== OUT =====
%s
===== OPTIONS =====
%s
===== OPS =====
%s
===== HISTORY =====
%s`, err, out, optionsStr, formattedOps, readHistory(filepath.Join(runDir, "history")))
		}
	}

	// Perform runs with the standard options.
	var names []string
	for i, opts := range standardOptions() {
		name := fmt.Sprintf("standard-%03d", i)
		names = append(names, name)
		t.Run(name, func(t *testing.T) {
			runOptions(t, opts)
		})
	}

	// Perform runs with random options.
	rng := rand.New(rand.NewSource(uint64(time.Now().UnixNano())))
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("random-%03d", i)
		names = append(names, name)
		t.Run(name, func(t *testing.T) {
			runOptions(t, randomOptions(rng))
		})
	}

	// Don't bother comparing output if we've already failed.
	if t.Failed() {
		return
	}

	// Read a history file, stripping out lines that begin with a comment.
	readHistory := func(name string) []string {
		historyPath := filepath.Join(metaDir, name, "history")
		data, err := ioutil.ReadFile(historyPath)
		require.NoError(t, err)
		lines := difflib.SplitLines(string(data))
		newLines := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.HasPrefix(line, "// ") {
				continue
			}
			newLines = append(newLines, line)
		}
		return newLines
	}

	base := readHistory(names[0])
	for i := 1; i < len(names); i++ {
		lines := readHistory(names[i])
		diff := difflib.UnifiedDiff{
			A:       base,
			B:       lines,
			Context: 5,
		}
		text, err := difflib.GetUnifiedDiffString(diff)
		require.NoError(t, err)
		if text != "" {
			// NB: We force an exit rather than using t.Fatal because the latter
			// will run another instance of the test if -count is specified, while
			// we're happy to exit on the first failure.
			fmt.Printf("diff %s/{%s,%s}\n%s\n", metaDir, names[0], names[i], text)
			os.Exit(1)
		}
	}
}

func readHistory(path string) string {
	history, err := ioutil.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("err: %v", err)
	}

	return string(history)
}
