// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Running on machines.

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// A machine represents a single system that runs tests and benchmarks.
type machine struct {
	name   string // name of machine
	kind   string // local, ssh, or gomote
	goos   string // target goos
	goarch string // target goarch
	cpu    int    // number of CPUs (cores)
	jobs   []*job // jobs to run on machine

	gomoteKind string // gomote type to use
	gomoteName string // gomote instance name
}

// A runMode controls the details of running a command.
type runMode int

const (
	_         runMode = 1 << iota
	runTrim           // trim spaces in output
	runStderr         // include stderr in output
)

// An executor runs commands.
type executor interface {
	// run has the same semantics as runLocal,
	// except that it need not handle runTrim.
	run(mode runMode, cmd ...string) (out string, err error)
}

// runLocal runs cmd on the local system according to mode.
// If the command fails, runLocal returns an empty output
// and an error message that contains both stdout and stderr.
// If mode has the runTrim bit set, runLocal trims leading and trailing spaces from the output.
// If mode has the runStderr bit set, then stderr is included in the output on success
// rather than being discarded.
func (l *Lab) runLocal(mode runMode, cmd ...string) (out string, err error) {
	out, err = l.exec.run(mode&^runTrim, cmd...)
	if mode&runTrim != 0 {
		out = strings.TrimSpace(out)
	}
	return out, err
}

// A localExec is an executor that runs commands locally.
// It is replaced in tests to avoid needing to run actual commands.
type localExec struct{}

func (*localExec) run(mode runMode, cmd ...string) (out string, err error) {
	if len(cmd) == 0 {
		return "", fmt.Errorf("missing command")
	}
	orig := cmd
	var env []string
	var dir string
	for len(cmd) > 0 && strings.Contains(cmd[0], "=") {
		if k, v, _ := strings.Cut(cmd[0], "="); k == "WD" {
			dir = v
			cmd = cmd[1:]
			continue
		}
		if env == nil {
			env = os.Environ()
		}
		env = append(env, cmd[0])
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		return "", fmt.Errorf("command entirely environment: %s", strings.Join(orig, " "))
	}

	c := exec.Command(cmd[0], cmd[1:]...)
	c.Env = env
	c.Dir = dir

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if mode&runStderr != 0 {
		c.Stderr = &stdout // merge stdout and stderr
	}
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s: %s\n%s%s", strings.Join(orig, " "), err, stdout.Bytes(), stderr.Bytes())
	}
	return stdout.String(), nil
}

// scanHosts reads the -host list and constructs the host, machine,
// and build data structures describing the hosts to be used.
func (l *Lab) scanHosts() error {
	machines := make(map[string]*machine)
	for _, hostname := range l.Hosts {
		name, _, _ := strings.Cut(hostname, ":")
		m := machines[name]
		if m == nil {
			m = &machine{name: name}
			machines[name] = m
			l.machines = append(l.machines, m)
		}
		l.hosts = append(l.hosts, &host{name: hostname, machine: m})
	}

	if err := parDo(l, l.machines, l.scanMachine); err != nil {
		// parDo already printed errors, so don't repeat them.
		return fmt.Errorf("scanning hosts failed")
	}

	builds := make(map[string]*build)
	for _, h := range l.hosts {
		goos := h.machine.goos
		goarch := h.machine.goarch
		var env, flags []string
		for _, opt := range strings.Split(h.name, ":")[1:] {
			k, v, ok := strings.Cut(opt, "=")
			if !ok || k == "" || v == "" {
				return fmt.Errorf("invalid config %q in host name: %v", opt, h.name)
			}
			switch {
			case k == "GOOS":
				goos = v
			case k == "GOARCH":
				goarch = v
			case k == strings.ToUpper(k):
				env = append(env, k+"="+v)
			default:
				flags = append(flags, "-"+k, v)
			}
		}
		key := fmt.Sprintf("%q %q %q %q", goos, goarch, env, flags)
		b := builds[key]
		if b == nil {
			b = &build{
				goos:   goos,
				goarch: goarch,
				env:    env,
				flags:  flags,
			}
			builds[key] = b
			l.builds = append(l.builds, b)
		}
		h.build = b
	}
	return nil
}

// scanMachine initializes the goos and goarch for a single machine.
//
// For ssh-able machines, scanMachine checks that ssh works,
// since it needs to ssh in to find the goos and goarch.
// The goos and goarch are cached in .benchlab, so that runs
// whose results are all cached need not ssh in at all.
//
// For gomotes, scanMachine determines the kind of gomote
// but does not create one yet, since the goos and goarch are
// evident from the name. This avoids creating gomotes
// (which can be very slow) when all the jobs from that gomote
// are already cached.
func (l *Lab) scanMachine(m *machine) error {
	if m.name == "local" {
		m.kind = "local"
		return l.scanArch(m)
	}
	for _, goos := range gooses {
		if strings.HasPrefix(m.name, goos+"-") {
			goos, goarch, _ := strings.Cut(m.name, "-")
			goarch, _, _ = strings.Cut(goarch, "-") // linux-amd64-longtest
			goarch, _, _ = strings.Cut(goarch, "_") // darwin-arm64_14
			m.kind = "gomote"
			m.goos = goos
			m.goarch = goarch
			return l.gomote.scan(l, m)
		}
	}
	m.kind = "ssh"
	if l.readArchCache(m) {
		return nil
	}
	if _, err := l.runRemote(m, 0, "date"); err != nil {
		return err
	}
	if err := l.scanArch(m); err != nil {
		return err
	}
	l.writeArchCache(m)
	return nil
}

// archCacheFile returns the path of the file caching m's goos and goarch.
func archCacheFile(m *machine) string {
	return ".benchlab/machine." + m.name + ".txt"
}

// readArchCache initializes m's goos and goarch from the arch cache,
// reporting whether it succeeded. The -a flag disables the cache.
func (l *Lab) readArchCache(m *machine) bool {
	if l.ForceRun {
		return false
	}
	data, err := l.fs.ReadFile(archCacheFile(m))
	if err != nil {
		return false
	}
	f := strings.Fields(string(data))
	if len(f) != 2 {
		return false
	}
	m.goos, m.goarch = f[0], f[1]
	return true
}

// writeArchCache saves m's goos and goarch to the arch cache.
func (l *Lab) writeArchCache(m *machine) {
	if err := l.fs.WriteFile(archCacheFile(m), []byte(m.goos+" "+m.goarch+"\n"), 0666); err != nil {
		l.log.Print(err)
	}
}

// scanArch determines the GOOS and GOARCH for the machine.
func (l *Lab) scanArch(m *machine) error {
	out, err := l.runRemote(m, runTrim, "uname")
	if err != nil {
		return err
	}
	var ok bool
	if m.goos, ok = goosByUname[out]; !ok {
		return fmt.Errorf("unknown uname: %s", out)
	}
	out, err = l.runRemote(m, runTrim, "uname", "-m")
	if err != nil {
		return err
	}
	if m.goarch, ok = goarchByUname[out]; !ok {
		return fmt.Errorf("unknown uname -m: %s", out)
	}
	return nil
}

// scanNumCPU determines the number of CPUs for the machine.
func (l *Lab) scanNumCPU(m *machine) error {
	var cmd []string
	switch m.goos {
	default:
		return fmt.Errorf("cannot count CPUs on GOOS=%s", m.goos)
	case "linux":
		cmd = []string{"nproc"}
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		cmd = []string{"sysctl", "hw.ncpu"}
	case "windows":
		cmd = []string{"wmic", "cpu", "get", "NumberOfCores"}
	}
	out, err := l.runRemote(m, 0, cmd...)
	if err != nil {
		return err
	}

	// Use last space-separated field, to skip leading chatter
	// like "hw.ncpu:" or "NumberOfCores".
	f := strings.Fields(out)
	if len(f) == 0 {
		return fmt.Errorf("'%s' on %s: no output", strings.Join(cmd, " "), m.name)
	}
	n, err := strconv.Atoi(f[len(f)-1])
	if err != nil {
		return fmt.Errorf("'%s' on %s: unexpected output:\n%s", strings.Join(cmd, " "), m.name, out)
	}
	m.cpu = n
	return nil
}

func (l *Lab) upload(m *machine, files []string) error {
	switch m.kind {
	case "ssh":
		_, err := l.runLocal(0, stringList("scp", files, m.name+":")...)
		return err
	case "gomote":
		for _, file := range files {
			if _, err := l.runLocal(0, "gomote", "put", m.gomoteName, file); err != nil {
				return err
			}
		}
	}
	return nil
}

// setupRunDirs creates a per-commit working directory on the remote machine
// and uploads testdata into it. On local machines the directories are created
// in .benchlab so the test binary does not run directly in $HOME.
func (l *Lab) setupRunDirs(m *machine) error {
	commits := make(map[string]bool)
	for _, j := range m.jobs {
		commits[j.commit] = true
	}
	for commit := range commits {
		dir := "benchlab.run." + commit
		switch m.kind {
		case "local":
			if err := l.fs.MkdirAll(dir, 0777); err != nil {
				return err
			}
			if tar := l.testdata[commit]; tar != "" {
				if _, err := l.runLocal(0, "tar", "-xzf", tar, "-C", dir); err != nil {
					return err
				}
			}
		case "ssh":
			if _, err := l.runRemote(m, 0, "mkdir", "-p", dir); err != nil {
				return err
			}
			if tar := l.testdata[commit]; tar != "" {
				l.log.Printf("uploading testdata for %s to %s", commit, m.name)
				if _, err := l.runLocal(0, "scp", tar, m.name+":"+dir+"/"); err != nil {
					return err
				}
				if _, err := l.runRemote(m, 0, "tar", "-xzf", dir+"/"+filepath.Base(tar), "-C", dir); err != nil {
					return err
				}
				l.runRemote(m, 0, "rm", dir+"/"+filepath.Base(tar))
			}
		case "gomote":
			// gomote run defaults CWD to the command's directory (e.g.
			// /usr/bin for mkdir), so use -dir to run in the workdir.
			if _, err := l.runLocal(0, "gomote", "run", "-dir", ".", m.gomoteName, "mkdir", "-p", dir); err != nil {
				return err
			}
			if tar := l.testdata[commit]; tar != "" {
				l.log.Printf("uploading testdata for %s to %s", commit, m.name)
				if _, err := l.runLocal(0, "gomote", "puttar", "-dir", dir, m.gomoteName, tar); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (l *Lab) runRemote(m *machine, mode runMode, cmd ...string) (out string, err error) {
	switch m.kind {
	case "ssh":
		// ssh joins its arguments with spaces and hands the result to a
		// shell on the remote system, so each argument must be quoted to
		// survive that round trip. Without this, a -test.bench regexp
		// containing shell metacharacters, as alternation and grouping do,
		// is a syntax error on the far end.
		cmd = stringList("ssh", m.name, shellQuote(cmd))
	case "gomote":
		// No quoting here: gomote run passes the command and its arguments
		// to the buildlet as a list, without a shell in between.
		cmd = stringList("gomote", "run", m.gomoteName, cmd)
	}
	return l.runLocal(mode, cmd...)
}

// shellQuote quotes each argument in cmd for interpretation by a POSIX shell,
// as when passing a command to ssh.
//
// Arguments that a shell would leave alone are returned unchanged, so that
// the commands benchlab ordinarily runs cross the wire byte for byte as they
// did before. That matters for remote systems whose shell is not a POSIX one:
// an ssh'able Windows machine, where benchlab runs "wmic cpu get NumberOfCores",
// sees the same words either way.
func shellQuote(cmd []string) []string {
	out := make([]string, len(cmd))
	for i, arg := range cmd {
		out[i] = shellQuoteArg(arg)
	}
	return out
}

// shellQuoteArg quotes a single argument for a POSIX shell.
func shellQuoteArg(arg string) string {
	if shellSafe(arg) {
		return arg
	}
	// Single quotes make the shell take everything literally.
	// A single quote cannot appear inside them, so end the quoted run,
	// write an escaped quote, and start a new one.
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// shellSafe reports whether arg means the same thing to a shell
// quoted or not, so that it can be passed through untouched.
func shellSafe(arg string) bool {
	if arg == "" {
		return false
	}
	for _, r := range arg {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
			continue
		}
		switch r {
		// Note that '=' is safe, which keeps a leading KEY=value argument
		// working as a shell variable assignment: quoting it would turn it
		// into the name of a command instead.
		case '@', '%', '+', '=', ':', ',', '.', '/', '-', '_':
			continue
		}
		return false
	}
	return true
}

// runRemoteDir runs a command on the machine with the working directory
// set to dir. For local execution it uses the WD= mechanism; for ssh it
// wraps the command in "cd dir && exec ..."; for gomote it uses -dir.
func (l *Lab) runRemoteDir(m *machine, dir string, mode runMode, cmd ...string) (out string, err error) {
	switch m.kind {
	case "local":
		cmd = append([]string{"WD=" + dir}, cmd...)
		return l.runLocal(mode, cmd...)
	case "ssh":
		// ssh joins its arguments and runs them through a remote shell,
		// so the cd and the command can travel as one shell command line.
		shell := "cd " + shellQuoteArg(dir) + " && exec " + strings.Join(shellQuote(cmd), " ")
		return l.runLocal(mode, "ssh", m.name, shell)
	case "gomote":
		cmd = stringList("gomote", "run", "-dir", dir, m.gomoteName, cmd)
		return l.runLocal(mode, cmd...)
	}
	return l.runLocal(mode, cmd...)
}

// A gomoter provides access to gomotes.
type gomoter struct {
	initOnce    sync.Once
	connectOnce sync.Once
	kinds       []string
	kindsErr    error
	motes       map[string][]string
	motesErr    error

	mu sync.Mutex // for connect
}

// gomoteKindsFile caches the output of "gomote create -list", so that runs
// whose results are all cached need not contact the gomote coordinator.
const gomoteKindsFile = ".benchlab/gomote-kinds.txt"

// init initializes the list of known gomote kinds and active available gomotes.
// The kinds are cached in .benchlab; the -a flag forces a refresh, which is
// also the fix if a cached kind becomes stale (for example, when a new
// darwin version replaces gotip-darwin-arm64_15).
func (g *gomoter) init(l *Lab) error {
	g.initOnce.Do(func() {
		if !l.ForceRun {
			if data, err := l.fs.ReadFile(gomoteKindsFile); err == nil {
				if kinds := strings.Fields(string(data)); len(kinds) > 0 {
					g.kinds = kinds
					return
				}
			}
		}
		// Load the set of gomote builds.
		out, err := l.runLocal(0, "gomote", "create", "-list")
		g.kindsErr = err
		g.kinds = strings.Fields(out)
		if err == nil {
			if err := l.fs.WriteFile(gomoteKindsFile, []byte(out), 0666); err != nil {
				l.log.Print(err)
			}
		}
	})
	return g.kindsErr
}

func (g *gomoter) connect(l *Lab, m *machine) error {
	g.connectOnce.Do(func() {
		// Create the benchlab group if it doesn't exist.
		// (If it does exist, ignore the error.)
		l.runLocal(0, "gomote", "group", "create", "benchlab")

		// List the existing motes for reuse,
		// but only in the benchlab group.
		out, err := l.runLocal(0, "gomote", "list")
		g.motesErr = err
		g.motes = make(map[string][]string)
		for line := range strings.Lines(out) {
			f := strings.Fields(line)
			if len(f) >= 3 && f[1] == "(benchlab)" {
				mote := f[0]
				kind := f[2]
				g.motes[kind] = append(g.motes[kind], mote)
			}
		}
	})
	if g.motesErr != nil {
		return g.motesErr
	}

	// If there is an existing gomote, take it.
	g.mu.Lock()
	motes := g.motes[m.gomoteKind]
	if len(motes) > 0 {
		m.gomoteName, g.motes[m.gomoteKind] = motes[0], motes[1:]
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	name, err := l.runLocal(runTrim, "gomote", "-group=benchlab", "create", m.gomoteKind)
	if err != nil {
		return err
	}
	m.gomoteName = name
	return nil
}

// scan finds the gomote kind that should be used for m.
func (g *gomoter) scan(l *Lab, m *machine) error {
	if err := g.init(l); err != nil {
		return err
	}
	want := "gotip-" + m.name
	wantLast := false
	switch {
	case m.name == "darwin-arm64" || m.name == "darwin-amd64":
		// darwin gomotes end in _14, _15 etc for the darwin version.
		// Take the last one in the list, which should be the biggest version.
		want += "_"
		wantLast = true

	case m.name == "linux-amd64" && slices.Contains(g.kinds, want+"_avx512"):
		// The plain gotip-linux-amd64 gomote is an older Xeon with no AVX-512,
		// so code that uses it dies with SIGILL there instead of running
		// slowly. Prefer the machine that can execute everything the compiler
		// might emit; ask for gotip-linux-amd64 by name to get the other one.
		want += "_avx512"
	}
	kind := ""
	for _, k := range g.kinds {
		if strings.HasPrefix(k, want) {
			kind = k
			if !wantLast {
				break
			}
		}
	}
	if kind == "" {
		return fmt.Errorf("no gomote kind for %s", m.name)
	}
	m.gomoteKind = kind
	return nil
}

// goosByUname maps "uname" output to GOOS.
var goosByUname = map[string]string{
	"Linux":     "linux",
	"Darwin":    "darwin",
	"FreeBSD":   "freebsd",
	"OpenBSD":   "openbsd",
	"NetBSD":    "netbsd",
	"DragonFly": "dragonfly",
	"Windows":   "windows",
}

// goarchByUname maps "uname -m" output to GOOS.
var goarchByUname = map[string]string{
	"x86_64":   "amd64",
	"amd64":    "amd64",
	"arm64":    "arm64",
	"aarch64":  "arm64",
	"arm":      "arm",
	"i386":     "386",
	"i686":     "386",
	"x86":      "386",
	"mips":     "mips",
	"mips64":   "mips64",
	"mips64le": "mips64le",
	"mipsle":   "mipsle",
	"ppc64":    "ppc64",
	"ppc64le":  "ppc64le",
	"s390x":    "s390x",
}

// gooses is the list of known GOOS values.
var gooses = []string{
	"darwin",
	"dragonfly",
	"freebsd",
	"linux",
	"netbsd",
	"openbsd",
	"solaris",
	"windows",
}
