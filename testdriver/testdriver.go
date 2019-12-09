// Package testdriver is a support package for plugins written using github.com/govim/govim
package testdriver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acarl005/stripansi"

	"github.com/govim/govim"
	"github.com/govim/govim/testsetup"
	"github.com/kr/pty"
	"github.com/rogpeppe/go-internal/semver"
	"github.com/rogpeppe/go-internal/testscript"
	"gopkg.in/retry.v1"
	"gopkg.in/tomb.v2"
)

const (
	KeyErrLog = "errLog"
)

// TODO - this code is a mess and needs to be fixed

type TestDriver struct {
	govimListener  net.Listener
	driverListener net.Listener
	govim          govim.Govim

	readLog *LockingBuffer
	log     io.Writer
	debug   Debug

	cmd *exec.Cmd

	name string

	plugin govim.Plugin

	quitVim    chan bool
	quitGovim  chan bool
	quitDriver chan bool

	doneQuitVim    chan bool
	doneQuitGovim  chan bool
	doneQuitDriver chan bool

	tomb tomb.Tomb

	closeLock sync.Mutex
	closed    bool
}

type Config struct {
	Name, GovimPath, TestHomePath, TestPluginPath string
	Debug
	ReadLog *LockingBuffer
	Log     io.Writer
	*testscript.Env
	Plugin govim.Plugin
}

type Debug struct {
	Enabled      bool
	VimLogLevel  int
	VimLogPath   string
	GovimLogPath string
}

func NewTestDriver(c *Config) (*TestDriver, error) {
	res := &TestDriver{
		quitVim:    make(chan bool),
		quitGovim:  make(chan bool),
		quitDriver: make(chan bool),

		doneQuitVim:    make(chan bool),
		doneQuitGovim:  make(chan bool),
		doneQuitDriver: make(chan bool),

		name: c.Name,

		plugin: c.Plugin,
	}
	if c.Log != nil {
		res.readLog = c.ReadLog
		res.log = c.Log
	} else {
		res.log = ioutil.Discard
	}
	gl, err := net.Listen("tcp4", "localhost:0")
	if err != nil {
		return nil, fmt.Errorf("failed to create listener for govim: %v", err)
	}
	dl, err := net.Listen("tcp4", ":0")
	if err != nil {
		return nil, fmt.Errorf("failed to create listener for driver: %v", err)
	}

	flav, cmd, err := testsetup.EnvLookupFlavorCommand()
	if err != nil {
		return nil, err
	}

	if err := copyDir(c.TestPluginPath, c.GovimPath); err != nil {
		return nil, fmt.Errorf("failed to copy %v to %v: %v", c.GovimPath, c.TestPluginPath, err)
	}

	var srcVimrc, dstVimrc string
	switch flav {
	case govim.FlavorVim:
		srcVimrc = filepath.Join(c.GovimPath, "cmd", "govim", "config", "minimal.vimrc")
		dstVimrc = filepath.Join(c.TestHomePath, ".vimrc")
	case govim.FlavorGvim:
		srcVimrc = filepath.Join(c.GovimPath, "cmd", "govim", "config", "minimal.gvimrc")
		dstVimrc = filepath.Join(c.TestHomePath, ".gvimrc")
	default:
		return nil, fmt.Errorf("need to add vimrc behaviour for flavour %v", flav)
	}

	// We use the minimal srcVimrc, augmented with some testdriver specific
	// commands.
	//
	// Before a test script starts, the files from the txtar archive are
	// extracted. Then govim (and gopls) are started. In some rare cases, gopls
	// can end up calculating diagnostics before the testscript has started.
	// Which means that it can be the case, because govim and Vim are already up
	// and running, that the first buffer actually gets used for quickfix as
	// opposed to, say, the first file we open.
	//
	// Instead below we copen then cclose in vimrc to reserve and establish a
	// definite buffer number for the quickfix window. The quickfix buffer will
	// always be buffer number 2 (vim creates a buffer when opening, copen then
	// creates a new window and buffer, the original buffer created when opening
	// vim gets used for the first file we open, say)
	vimrcBytes, err := ioutil.ReadFile(srcVimrc)
	if err != nil {
		return nil, fmt.Errorf("failed to read from src vimrc %v: %v", srcVimrc, err)
	}
	vimrc := bytes.NewBuffer(vimrcBytes)
	vimrc.WriteString("\n")
	vimrc.WriteString("copen\n")
	vimrc.WriteString("cclose\n")

	if err := ioutil.WriteFile(dstVimrc, vimrc.Bytes(), 0666); err != nil {
		return nil, fmt.Errorf("failed to write to dst vimrc %v: %v", dstVimrc, err)
	}

	res.govimListener = gl
	res.driverListener = dl

	c.Env.Vars = append(c.Env.Vars,
		"GOVIMTEST_SOCKET="+res.govimListener.Addr().String(),
		"GOVIMTESTDRIVER_SOCKET="+res.driverListener.Addr().String(),
	)

	vimCmd := cmd
	if e := os.Getenv("VIM_COMMAND"); e != "" {
		vimCmd = strings.Fields(e)
	}

	if c.Debug.Enabled {
		res.debug = c.Debug
		vimCmd = append(vimCmd, fmt.Sprintf("-V%d%s", c.Debug.VimLogLevel, c.VimLogPath))
	}

	res.cmd = exec.Command(vimCmd[0], vimCmd[1:]...)
	res.cmd.Env = c.Env.Vars
	res.cmd.Dir = c.Env.WorkDir

	if res.debug.Enabled {
		envlist := ""
		for _, e := range c.Env.Vars {
			if e != ":=:" {
				envlist += " " + strings.ReplaceAll(e, " ", `\ `)
			}
		}

		fmt.Printf("Test command:\n==========================\npushd %s && %s %s && popd\n==========================\n", c.Env.WorkDir, envlist, strings.Join(res.cmd.Args, " "))
	}

	return res, nil
}

func (d *TestDriver) Logf(format string, a ...interface{}) {
	fmt.Fprintf(d.log, format+"\n", a...)
}
func (d *TestDriver) LogStripANSI(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for {
		ok := scanner.Scan()
		if !ok {
			if scanner.Err() != nil {
				fmt.Fprintf(d.log, "Erroring copying log: %+v\n", scanner.Err())
			}
			return
		}
		fmt.Fprint(d.log, stripansi.Strip(scanner.Text()))
	}
}

func copyDir(dst, src string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		switch path {
		case filepath.Join(src, ".git"), filepath.Join(src, "cmd", "govim", ".bin"):
			return filepath.SkipDir
		}
		rel := strings.TrimPrefix(path, src)
		if strings.HasPrefix(rel, string(os.PathSeparator)) {
			rel = strings.TrimPrefix(rel, string(os.PathSeparator))
		}
		dstpath := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(dstpath, 0777)
		}
		return copyFile(dstpath, path)
	})
}

func copyFile(dst, src string) error {
	r, err := os.Open(src)
	if err != nil {
		return err
	}
	w, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	r.Close()
	return w.Close()
}

func (d *TestDriver) Run() error {
	d.tombgo(d.runVim)
	if err := d.listenGovim(); err != nil {
		return err
	}
	select {
	case <-d.tomb.Dying():
		return d.tomb.Err()
	case <-d.govim.Initialized():
	}
	return nil
}

func (d *TestDriver) Wait() error {
	return d.tomb.Wait()
}

func (d *TestDriver) runVim() error {
	thepty, err := pty.Start(d.cmd)
	if err != nil {
		close(d.doneQuitVim)
		err := fmt.Errorf("failed to start %v: %v", strings.Join(d.cmd.Args, " "), err)
		d.Logf("error: %+v", err)
		return err
	}
	d.tombgo(func() error {
		defer func() {
			thepty.Close()
			close(d.doneQuitVim)
		}()
		if err := d.cmd.Wait(); err != nil {
			select {
			case <-d.quitVim:
			default:
				return fmt.Errorf("vim exited: %v", err)
			}
		}
		return nil
	})

	if d.debug.Enabled {
		d.LogStripANSI(thepty)
	} else {
		io.Copy(ioutil.Discard, thepty)
	}

	return nil
}

func (d *TestDriver) Close() {
	d.closeLock.Lock()
	if d.closed {
		d.closeLock.Unlock()
		return
	}
	d.closed = true
	d.closeLock.Unlock()
	select {
	case <-d.doneQuitVim:
	default:
		close(d.quitVim)
	}
	select {
	case <-d.doneQuitGovim:
	default:
		close(d.quitGovim)
	}
	select {
	case <-d.doneQuitDriver:
	default:
		close(d.quitDriver)
	}
	select {
	case <-d.doneQuitVim:
	default:
		func() {
			defer func() {
				if r := recover(); r != nil && r != govim.ErrShuttingDown {
					panic(r)
				}
			}()
			_, err := d.govim.Schedule(func(g govim.Govim) (err error) {
				g.ChannelEx("qall!")
				return
			})
			if err != nil {
				panic(err)
			}
		}()
		<-d.doneQuitVim
	}
	select {
	case <-d.doneQuitDriver:
	default:
		d.driverListener.Close()
		<-d.doneQuitDriver
	}
}

func (d *TestDriver) tombgo(f func() error) {
	d.tomb.Go(func() error {
		err := f()
		if err != nil {
			fmt.Printf(">>> %v\n", err)
			if d.debug.Enabled {
				fmt.Printf("Govim debug logs:\n==========================\n")
				f, err := os.Open(d.debug.GovimLogPath)
				if err != nil {
					fmt.Printf("Error opening debug logs: %+v\n", err)
				} else {
					io.Copy(os.Stdout, f)
				}
				fmt.Printf("==========================\n")
			}
			d.Close()
		}
		return err
	})
}

func (d *TestDriver) listenGovim() error {
	good := false
	defer func() {
		if !good {
			close(d.doneQuitGovim)
			close(d.doneQuitDriver)
		}
	}()
	conn, err := d.govimListener.Accept()
	if err != nil {
		select {
		case <-d.quitGovim:
			return nil
		default:
			return fmt.Errorf("failed to accept connection on %v: %v", d.govimListener.Addr(), err)
		}
	}
	if err := d.govimListener.Close(); err != nil {
		return fmt.Errorf("failed to close listener: %v", err)
	}

	var log io.Writer = ioutil.Discard
	if d.log != nil {
		log = d.log
	}
	g, err := govim.NewGovim(d.plugin, conn, conn, log, &d.tomb)
	if err != nil {
		return fmt.Errorf("failed to create govim: %v", err)
	}
	good = true
	d.govim = g
	d.tombgo(d.listenDriver)
	d.tombgo(d.runGovim)

	return nil
}

func (d *TestDriver) runGovim() error {
	defer close(d.doneQuitGovim)
	if err := d.govim.Run(); err != nil {
		select {
		case <-d.quitGovim:
		default:
			return fmt.Errorf("govim Run failed: %v", err)
		}
	}
	return nil
}

func (d *TestDriver) listenDriver() error {
	defer close(d.doneQuitDriver)
	err := d.govim.DoProto(func() error {
	Accept:
		for {
			conn, err := d.driverListener.Accept()
			if err != nil {
				select {
				case <-d.quitDriver:
					break Accept
				default:
					panic(fmt.Errorf("failed to accept connection to driver on %v: %v", d.driverListener.Addr(), err))
				}
			}
			dec := json.NewDecoder(conn)
			var args []interface{}
			if err := dec.Decode(&args); err != nil {
				panic(fmt.Errorf("failed to read command for driver: %v", err))
			}
			cmd := args[0]
			res := []interface{}{""}
			add := func(err error, is ...interface{}) {
				toAdd := []interface{}{""}
				if err != nil {
					toAdd[0] = err.Error()
				} else {
					toAdd = append(toAdd, is...)
				}
				res = append(res, toAdd)
			}
			schedule := func(f func(govim.Govim) error) chan struct{} {
				// If we get an error or panic whilst trying to issue a command
				// from a script something has gone very wrong. Typically this will
				// happen when govim is shutting down for some reason. This should
				// never happen but did, for example, when the race detector found
				// a race in gopls which caused gopls to panic and quit, something
				// which triggers govim to shutdown (correctly).
				defer func() {
					r := recover()
					if r == nil {
						return
					}
					panic(fmt.Errorf("we are in test: %v\n\nLog is: \n%s\n\nPanic was: %v", d.name, d.readLog.Bytes(), r))
				}()
				ch, err := d.govim.Schedule(f)
				if err != nil {
					panic(err)
				}
				return ch
			}
			switch cmd {
			case "redraw":
				var force string
				if len(args) == 2 {
					force = args[1].(string)
				}
				<-schedule(func(g govim.Govim) error {
					add(g.ChannelRedraw(force == "force"))
					return nil
				})
			case "ex":
				expr := args[1].(string)
				<-schedule(func(g govim.Govim) error {
					add(g.ChannelEx(expr))
					return nil
				})
			case "normal":
				expr := args[1].(string)
				<-schedule(func(g govim.Govim) error {
					add(g.ChannelNormal(expr))
					return nil
				})
			case "expr":
				expr := args[1].(string)
				<-schedule(func(g govim.Govim) error {
					resp, err := g.ChannelExpr(expr)
					add(err, resp)
					return nil
				})
			case "call":
				fn := args[1].(string)
				<-schedule(func(g govim.Govim) error {
					resp, err := g.ChannelCall(fn, args[2:]...)
					add(err, resp)
					return nil
				})
			default:
				panic(fmt.Errorf("don't yet know how to handle %v", cmd))
			}
			enc := json.NewEncoder(conn)
			if err := enc.Encode(res); err != nil {
				panic(fmt.Errorf("failed to encode response %v: %v", res, err))
			}
			conn.Close()
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("%v", err)
	}
	return nil
}

// Vim is a sidecar that effectively drives Vim via a simple JSON-based
// API
func Vim() (exitCode int) {
	logFile := os.Getenv("GOVIMTESTDRIVER_LOG")
	var l io.Writer
	if logFile != "" {
		var err error
		l, err = os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			panic(fmt.Sprintf("Could not open log file: %+v", err))
		}
	} else {
		l = ioutil.Discard
	}
	log := func(format string, args ...interface{}) {
		fmt.Fprintf(l, "[vim test client] "+format, args...)
	}
	log("logging enabled")

	defer func() {
		r := recover()
		if r == nil {
			return
		}
		exitCode = -1
		fmt.Fprintln(os.Stderr, r)
		log("panic with error: %+v", r)
	}()

	ef := func(format string, args ...interface{}) {
		log(format, args...)
		panic(fmt.Sprintf(format, args...))
	}

	fs := flag.NewFlagSet("vim", flag.PanicOnError)
	bang := fs.Bool("bang", false, "expect command to fail")
	indent := fs.Bool("indent", false, "pretty indent resulting JSON")
	stringout := fs.Bool("stringout", false, "print resulting string rather than JSON encoded version of string")

	log("starting vim driver client and parsing flags...")

	fs.Parse(os.Args[1:])
	args := fs.Args()
	fn := args[0]
	var jsonArgs []string
	for i, a := range args {
		if i <= 1 {
			uq, err := strconv.Unquote("\"" + a + "\"")
			if err != nil {
				ef("failed to unquote %q: %v", a, err)
			}
			jsonArgs = append(jsonArgs, strconv.Quote(uq))
		} else {
			var buf bytes.Buffer
			json.HTMLEscape(&buf, []byte(a))
			jsonArgs = append(jsonArgs, buf.String())
		}
	}
	jsonArgString := "[" + strings.Join(jsonArgs, ", ") + "]"
	var i []interface{}
	if err := json.Unmarshal([]byte(jsonArgString), &i); err != nil {
		ef("failed to json Unmarshal %q: %v", jsonArgString, err)
	}
	switch fn {
	case "redraw":
		// optional argument of force
		switch l := len(args[1:]); l {
		case 0:
		case 1:
			if args[1] != "force" {
				ef("unknown argument %q to redraw", args[1])
			}
		default:
			ef("redraw has a single optional argument: force; we saw %v", l)
		}
	case "ex", "normal", "expr":
		switch l := len(args[1:]); l {
		case 1:
			if _, ok := i[1].(string); !ok {
				ef("%v takes a string argument; saw %T", fn, i[1])
			}
		default:
			ef("%v takes a single argument: we saw %v", fn, l)
		}
	case "call":
		switch l := len(args[1:]); l {
		case 1:
			// no args
			if _, ok := i[1].(string); !ok {
				ef("%v takes a string as its first argument; saw %T", fn, i[1])
			}
		case 2:
			if _, ok := i[1].(string); !ok {
				ef("%v takes a string as its first argument; saw %T", fn, i[1])
			}
			vs, ok := i[2].([]interface{})
			if !ok {
				ef("%v takes a slice of values as its second argument; saw %T", fn, i[2])
			}
			// on the command line we require the args to be specified as an array
			// for ease of explanation/documentation, but now we splat the slice
			i = append(i[:2], vs...)
		default:
			ef("%v takes a two arguments: we saw %v", fn, l)
		}
	}
	if bs, err := json.Marshal(i); err != nil {
		ef("failed to remarshal json args: %v", err)
	} else {
		jsonArgString = string(bs)
	}
	addr := os.Getenv("GOVIMTESTDRIVER_SOCKET")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		ef("failed to connect to driver on %v: %v", addr, err)
	}
	if _, err := fmt.Fprintln(conn, jsonArgString); err != nil {
		ef("failed to send command %q to driver on: %v", jsonArgString, err)
	}
	dec := json.NewDecoder(conn)
	var resp []interface{}
	if err := dec.Decode(&resp); err != nil {
		ef("failed to decode response: %v", err)
	}
	if resp[0] != "" {
		// this is a protocol-level error
		ef("got error response: %v", resp[0])
	}
	// resp[1] will be a []intferface{} where the first
	// element will be a Vim-level error
	vimResp := resp[1].([]interface{})
	if err := vimResp[0].(string); err != "" {
		// this was a vim-level error
		if !*bang {
			ef("unexpected command error: %v", err)
		}
		fmt.Fprintln(os.Stderr, err)
	}
	if len(vimResp) == 2 {
		if *bang {
			ef("unexpected command success")
		}
		if *stringout {
			switch vimResp[1].(type) {
			case string:
				fmt.Print(vimResp[1])
			default:
				ef("response type is %T, not string", vimResp[1])
			}
		} else {
			enc := json.NewEncoder(os.Stdout)
			if *indent {
				enc.SetIndent("", "  ")
			}
			if err := enc.Encode(vimResp[1]); err != nil {
				ef("failed to format output of JSON: %v", err)
			}
		}
	}
	return 0
}

// Sleep is a convenience function for those odd occasions when you
// need to drop in a sleep, e.g. waiting for CursorHold to trigger
func Sleep(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("sleep does not support neg")
	}
	if len(args) != 1 {
		ts.Fatalf("sleep expects a single argument; got %v", len(args))
	}
	d, err := time.ParseDuration(args[0])
	if err != nil {
		ts.Fatalf("failed to parse duration %q: %v", args[0], err)
	}
	time.Sleep(d)
}

func Condition(cond string) (bool, error) {
	envf, cmd, err := testsetup.EnvLookupFlavorCommand()
	if err != nil {
		return false, err
	}
	var f govim.Flavor
	switch {
	case strings.HasPrefix(cond, govim.FlavorVim.String()):
		f = govim.FlavorVim
	case strings.HasPrefix(cond, govim.FlavorGvim.String()):
		f = govim.FlavorGvim
	default:
		return false, fmt.Errorf("unknown condition %v", cond)
	}
	v := strings.TrimPrefix(cond, f.String())
	if envf != f {
		return false, nil
	}
	if v == "" {
		return true, nil
	}
	if v[0] != ':' {
		return false, fmt.Errorf("failed to find version separator")
	}
	v = v[1:]
	if !semver.IsValid(v) {
		return false, fmt.Errorf("%v is not a valid semver version", v)
	}
	switch f {
	case govim.FlavorVim, govim.FlavorGvim:
		cmd := cmd.BuildCommand("-v", "--version")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return false, fmt.Errorf("failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, out)
		}
		version, err := parseVimVersion(out)
		if err != nil {
			return false, err
		}
		return semver.Compare(version, v) >= 0, nil
	}

	panic("should not reach here")
}

func parseVimVersion(b []byte) (string, error) {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	av := "v"
	av += strings.Fields(lines[0])[4] // 5th element is the major.minor

	// Depending on OS/build, the patch versions are printed on different lines
	var patch string
	for _, line := range lines {
		if strings.Contains(line, ": ") {
			patch = strings.Fields(line)[2]
			patchI := strings.Index(patch, "-")
			if patchI == -1 {
				return "", fmt.Errorf("failed to parse patch version from %v", patch)
			}
			patch = patch[patchI+1:]
			break
		}
	}
	av += "." + patch
	if !semver.IsValid(av) {
		return "", fmt.Errorf("failed to calculate valid Vim version from %q; got %v", b, av)
	}

	return av, nil
}

type LockingBuffer struct {
	lock          sync.Mutex
	und           bytes.Buffer
	NextSearchInx int
}

func (l *LockingBuffer) Write(p []byte) (n int, err error) {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.und.Write(p)
}

func (l *LockingBuffer) Bytes() []byte {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.und.Bytes()
}

func ErrLogMatch(ts *testscript.TestScript, neg bool, args []string) {
	errLogV := ts.Value(KeyErrLog)
	if errLogV == nil {
		ts.Fatalf("errlogmatch failed to find %v value", KeyErrLog)
	}
	errLog, ok := errLogV.(*LockingBuffer)
	if !ok {
		ts.Fatalf("errlogmatch %v was not the right type", KeyErrLog)
	}

	defaultWait := os.Getenv("GOVIM_ERRLOGMATCH_WAIT")
	if defaultWait == "" {
		defaultWait = "30s"
	}

	fs := flag.NewFlagSet("errlogmatch", flag.ContinueOnError)
	fStart := fs.Bool("start", false, "search from beginning, not last snapshot")
	fPeek := fs.Bool("peek", false, "do not adjust the NextSearchInx field on the errlog")
	fWait := fs.String("wait", "", "retry (with exp backoff) until this time period has elapsed")
	fCount := fs.Int("count", -1, "number of instances to wait for/match")
	if err := fs.Parse(args); err != nil {
		ts.Fatalf("errlogmatch: failed to parse args %v: %v", args, err)
	}

	var waitSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "wait" {
			waitSet = true
		}
	})

	if neg && waitSet {
		ts.Fatalf("-wait is not compatible with negating the command")
	}
	if !neg && *fWait == "" {
		fWait = &defaultWait
	}

	switch {
	case *fCount < 0:
		// not active
	default:
		if neg {
			ts.Fatalf("cannot use -count with negated match")
		}
	}

	if len(fs.Args()) != 1 {
		ts.Fatalf("errlogmatch expects a single argument, the regexp to search for")
	}

	reg, err := regexp.Compile(fs.Args()[0])
	if err != nil {
		ts.Fatalf("errlogmatch failed to regexp.Compile %q: %v", fs.Args()[0], err)
	}

	wait := time.Duration(0)
	if *fWait != "" {
		pwait, err := time.ParseDuration(*fWait)
		if err != nil {
			ts.Fatalf("errlogmatch: failed to parse -maxwait duration %q: %v", *fWait, err)
		}
		wait = pwait
	}

	strategy := retry.LimitTime(wait,
		retry.Exponential{
			Initial: 10 * time.Millisecond,
			Factor:  1.5,
		},
	)

	// If we are not waiting, limit to one-shot (i.e. effectively negate the effect of
	// the retry
	if *fWait == "" {
		strategy = retry.LimitCount(1, strategy)
	}

	var nextInx int
	if !*fPeek {
		defer func() {
			errLog.NextSearchInx = nextInx
		}()
	}
	var matches [][]int
	var searchStart int
	for a := retry.Start(strategy, nil); a.Next(); {
		toSearch := errLog.Bytes()
		nextInx = len(toSearch)
		if !*fStart {
			searchStart = errLog.NextSearchInx
		}
		matches = reg.FindAllIndex(toSearch[searchStart:], -1)
		if *fCount >= 0 {
			if len(matches) != *fCount {
				continue
			}
			if len(matches) > 0 {
				nextInx = matches[len(matches)-1][1] + searchStart // End of last match
			}
			return
		}
		if matches != nil {
			nextInx = matches[len(matches)-1][1] + searchStart // End of last match
			if neg {
				ts.Fatalf("errlogmatch found unexpected match (%q)", toSearch)
			}
			// we found a match or the correct count and were expecting it
			return
		}
	}

	if *fCount >= 0 {
		ts.Fatalf("expected %v matches; found %v", *fCount, len(matches))
	}
	if !neg {
		ts.Fatalf("errlogmatch failed to find match")
	}
	// we didn't find a match, but this is expected
}
