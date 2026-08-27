package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/michiTrader/iash/internal/blueprint"
	"github.com/michiTrader/iash/internal/surface"
)

// The NDJSON protocol: one JSON object per line, in each direction.
//
// NDJSON was chosen over a framed binary protocol for one reason that outweighs
// the efficiency argument: the transcript of a session is readable with `cat`. A
// protocol whose traffic can only be inspected by a tool written for it is a
// protocol nobody debugs, and this project's whole premise is that the expensive
// failures are the silent ones. `tee` on the socket is a complete debugger.
//
// The message type IS the CLI path joined with dots (surface.ProtocolType), so
// there is no wire vocabulary to keep in step with the command vocabulary. See
// docs/design/20-use-cases.md §20.12.

// maxLineBytes caps a single request line at 1 MiB.
//
// A cap is not optional. A reader that grows to hold whatever arrives makes one
// client able to exhaust the server's memory by never sending a newline, and the
// death is an OOM kill with no request in flight to blame. 1 MiB is far above any
// legitimate request — the largest declared parameter is a prompt — and far below
// a number that hurts.
const maxLineBytes = 1 << 20

// protoRequest is one line from a client.
//
// `id` is echoed back on the response and is the client's, not ours. A client
// with several requests in flight, reading replies from a goroutine, cannot pair
// them up otherwise; and errors about a line we could not even parse have to
// carry SOMETHING, which is why an empty id is legal rather than rejected.
type protoRequest struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Params map[string]any `json:"params"`
}

// protoResponse is one line back.
//
// `ok` is explicit rather than inferred from the presence of `error`. Those two
// encodings disagree the first time a server omits an empty error object or a
// client checks the wrong one, and the disagreement reads as success. One boolean
// is one thing to check and cannot be ambiguous.
type protoResponse struct {
	ID     string      `json:"id"`
	OK     bool        `json:"ok"`
	Result any         `json:"result,omitempty"`
	Error  *protoError `json:"error,omitempty"`
}

// protoError carries a machine code AND a human sentence.
//
// The code is what a client branches on; the message is what ends up in
// somebody's terminal at 2am. Sending only the code makes every client
// reimplement the English, and they will not agree. `fix` carries the same kind
// of remedy `run why` prints, for the same reason: a diagnosis that does not say
// what to do next makes the reader guess.
type protoError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Fix     []string `json:"fix,omitempty"`
}

// Error codes. These are a closed set on purpose: a client has to be able to tell
// "you asked wrongly" (its own bug, retrying will not help) from "this build
// cannot do that yet" (not its bug, and retrying after an upgrade will help).
// Collapsing them into one generic failure makes every client either retry
// forever or give up permanently, and both are wrong half the time.
const (
	errMalformed      = "malformed"       // not a JSON object
	errUnknownType    = "unknown_type"    // not a protocol message in this surface
	errBadParams      = "bad_params"      // the type is real, the arguments are not
	errNotImplemented = "not_implemented" // declared in the surface, no executor yet
	errFailed         = "failed"          // the command ran and could not succeed
	errLineTooLong    = "line_too_long"   // over maxLineBytes; the stream is unusable
)

// helloMsg is written before the server reads anything.
//
// The client needs to know what it is talking to BEFORE it commits to a request,
// and there is no other way to learn it: `schema` describes the agent tools,
// which is a different set from the protocol types (`run attach` and the three
// `inbox` replies are on the wire and are deliberately not tools). So the
// protocol's own capability set is discoverable here and nowhere else.
//
// `implemented` is a subset of `types` and is sent for an honest reason: most of
// this surface is declared and has no executor yet. Without the list, a client
// discovers that one type at a time by sending a request and reading a failure,
// which makes a permanent state look like a transient error.
type helloMsg struct {
	Type           string   `json:"type"`
	Version        string   `json:"version"`
	SurfaceVersion int      `json:"surface_version"`
	Types          []string `json:"types"`
	Implemented    []string `json:"implemented"`
}

// protoHandler runs one request. It returns a result to marshal, or an error.
type protoHandler func(params map[string]any) (any, error)

// protoHandlers holds the implementations that exist.
//
// This map is NOT the set of accepted types — that is derived from the registry
// by surface.ProtocolCommands, and a type with no entry here is answered
// `not_implemented` rather than `unknown_type`. The distinction is the whole
// point: one means the client is wrong, the other means this build is behind, and
// a client cannot react correctly to a failure that conflates them.
//
// Keys are checked against the registry by a test, because a typo here is
// invisible: the handler simply never runs, and the capability reports itself
// unimplemented while its code sits in the binary.
var protoHandlers = map[string]protoHandler{
	"schema":             handleSchema,
	"blueprint.validate": handleBlueprintValidate,
}

// serveConn runs the protocol over one reader/writer pair.
//
// Taking io.Reader and io.Writer rather than a net.Conn is what makes the
// protocol testable without a socket, and it is also what makes stdio and a unix
// socket the same code path instead of two implementations that drift. The only
// difference between `iash serve` and `iash serve --listen` is where these two
// come from.
//
// Requests on one connection are handled STRICTLY IN ORDER. Handling them
// concurrently would let a `state set` and the `state get` after it be answered
// in the order they finished rather than the order they were sent, so a client
// that wrote a value could read back the old one — a lost update produced by the
// server, not by the race the CAS in ADR-0006 was built to catch.
func serveConn(r io.Reader, w io.Writer) error {
	enc := json.NewEncoder(w)

	if err := enc.Encode(protoHello()); err != nil {
		// Failing to send the hello is fatal for this connection: the client is
		// entitled to assume the first line tells it the surface version, and one
		// that never arrives leaves it guessing which vocabulary it may use.
		return fmt.Errorf("announce the surface: %w", err)
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			// Blank lines are skipped rather than reported. Plenty of clients emit
			// one when flushing, and answering it with an error would make every
			// well-behaved client generate spurious failures in its own logs.
			continue
		}
		if err := enc.Encode(handleLine(line)); err != nil {
			// A write that fails means the client is gone or the pipe broke. There
			// is nowhere to report it TO, so it ends the connection.
			return fmt.Errorf("write a response: %w", err)
		}
	}

	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// The line is reported and then the connection ENDS, because after a
			// truncated line the stream position is unknown: the tail of the
			// oversized request would be read as the next one and dispatched as
			// whatever it happened to parse as. Continuing would turn one
			// oversized request into an arbitrary command nobody sent.
			_ = enc.Encode(protoResponse{OK: false, Error: &protoError{
				Code: errLineTooLong,
				Message: fmt.Sprintf("a request line exceeded %d bytes, so the "+
					"connection is closing: after a truncated line the rest of it "+
					"would be read as the next request and dispatched as whatever "+
					"it parsed as", maxLineBytes),
				Fix: []string{"send one JSON object per line and keep it under 1 MiB"},
			}})
			return fmt.Errorf("request line over %d bytes", maxLineBytes)
		}
		return fmt.Errorf("read a request: %w", err)
	}
	return nil
}

// handleLine turns one line into one response and never returns an error, because
// every failure below is the client's and belongs on the wire where the client
// can read it. A protocol server that drops a connection over a bad request makes
// one typo cost every other in-flight request on that connection.
func handleLine(line string) protoResponse {
	var req protoRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		// No id is available here — the line did not parse — so the response
		// carries an empty one. That is why an empty id is legal on the wire.
		return protoResponse{OK: false, Error: &protoError{
			Code: errMalformed,
			Message: fmt.Sprintf("this line is not a JSON object: %v. Every line "+
				"is one request: {\"id\":..., \"type\":..., \"params\":{...}}", err),
			Fix: []string{"iash schema"},
		}}
	}

	c := surface.LookupProtocol(req.Type)
	if c == nil {
		return protoResponse{ID: req.ID, OK: false, Error: unknownTypeError(req.Type)}
	}

	if err := validateParams(*c, req.Params); err != nil {
		return protoResponse{ID: req.ID, OK: false, Error: &protoError{
			Code:    errBadParams,
			Message: err.Error(),
			Fix:     []string{"iash schema"},
		}}
	}

	h, ok := protoHandlers[req.Type]
	if !ok {
		return protoResponse{ID: req.ID, OK: false, Error: &protoError{
			Code: errNotImplemented,
			Message: fmt.Sprintf("%s is declared in surface v%d and this build has "+
				"no executor for it. The request was well formed; retrying will not "+
				"help until the capability lands.", c.CLI(), c.Since),
			Fix: []string{"iash surface"},
		}}
	}

	res, err := h(req.Params)
	if err != nil {
		return protoResponse{ID: req.ID, OK: false, Error: &protoError{
			Code:    errFailed,
			Message: err.Error(),
		}}
	}
	return protoResponse{ID: req.ID, OK: true, Result: res}
}

// unknownTypeError distinguishes a type that does not exist from one that exists
// and is deliberately not on the wire.
//
// Answering both with a flat "unknown type" sends the second client hunting for a
// typo it never made — the same failure main.go's fallthrough exists to prevent
// on the CLI. The honest answer matters here more, not less: `serve`, `design`
// and `agent tool policy` are absent from the protocol as a security boundary
// (§20.12), and a client is entitled to be told that rather than left to conclude
// the server is broken.
func unknownTypeError(t string) *protoError {
	if c := surface.Lookup(strings.Split(t, ".")...); c != nil {
		return &protoError{
			Code: errUnknownType,
			Message: fmt.Sprintf("%q is a real capability (iash %s) and is not "+
				"exposed to the protocol. That is deliberate, not missing: the "+
				"operator-side capabilities are held off the wire so a socket "+
				"client cannot change the rules a run is judged by.", t, c.CLI()),
			Fix: []string{"iash " + c.CLI()},
		}
	}
	return &protoError{
		Code: errUnknownType,
		Message: fmt.Sprintf("%q is not a message type in surface v%d. The type is "+
			"the CLI path with dots: `run why` is `run.why`.", t, surface.SurfaceVersion),
		Fix: []string{"iash schema"},
	}
}

// validateParams rejects anything the declared schema does not describe.
//
// Strict rather than lenient, and the reason is a specific silent failure.
// `run prompt` carries `if_seq`, the compare-and-swap of ADR-0006. A client that
// misspells it and is ignored believes its write was conditional when it was
// last-write-wins, so the lost update the CAS exists to catch happens anyway and
// the log records the write as intended. Every optional parameter in this surface
// is a guard of that shape: ignoring an unknown one turns a request the client
// thought was safe into one that is not.
//
// Missing REQUIRED parameters are caught for the cheaper version of the same
// reason — `budget` absent is a run with no spend ceiling.
func validateParams(c surface.Cmd, params map[string]any) error {
	declared := map[string]surface.Param{}
	for _, pp := range c.WireParams() {
		declared[pp.Name] = pp
	}

	// Sorted so the message is the same on every run: an error that names its
	// offending keys in map order is an error nobody can write a test against.
	var unknown []string
	for name := range params {
		if _, ok := declared[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		known := make([]string, 0, len(declared))
		for name := range declared {
			known = append(known, name)
		}
		sort.Strings(known)
		return fmt.Errorf("%s does not take %s. It takes: %s. "+
			"Unknown parameters are refused rather than ignored: a misspelled "+
			"guard (if_seq, budget) that is silently dropped makes a request the "+
			"client believed was safe unsafe, and the log records it as intended",
			c.ProtocolType(), strings.Join(unknown, ", "), strings.Join(known, ", "))
	}

	for _, pp := range c.WireParams() {
		v, present := params[pp.Name]
		// A JSON null is treated as absent rather than as a value. Clients that
		// serialize omitted fields as null are common, and rejecting them would
		// fail requests that are correct in every way that matters.
		if v == nil {
			present = false
		}
		if !present {
			if pp.Required {
				return fmt.Errorf("%s requires %q (%s)", c.ProtocolType(), pp.Name, pp.Desc)
			}
			continue
		}
		if err := checkType(c, pp, v); err != nil {
			return err
		}
	}
	return nil
}

// checkType refuses a value of the wrong JSON type instead of coercing it.
//
// Coercion is what makes `{"budget": "2.00"}` a run with a ceiling of zero: the
// string does not parse as a number, the zero value looks deliberate, and the
// most cautious-looking request becomes the most dangerous one. The same argument
// as TestRunStartRefusesANonPositiveBudget, one layer out.
func checkType(c surface.Cmd, pp surface.Param, v any) error {
	want := pp.Type
	var okType bool
	switch want {
	case "bool":
		_, okType = v.(bool)
	case "number":
		_, okType = v.(float64)
	default:
		want = "string"
		_, okType = v.(string)
	}
	if !okType {
		return fmt.Errorf("%s: %q must be a %s, got %T. Values are not coerced: "+
			"a budget of \"2.00\" read as a number would become 0, which looks "+
			"deliberate and is the one ceiling that cannot have a default",
			c.ProtocolType(), pp.Name, want, v)
	}

	// An out-of-enum value must be refused too. Falling back to the default would
	// silently answer a different question than the one asked: `on_busy: "abort"`
	// resolving to `queue` means the client asked to reject the injection and got
	// it applied.
	if len(pp.Enum) > 0 {
		s, _ := v.(string)
		for _, allowed := range pp.Enum {
			if s == allowed {
				return nil
			}
		}
		return fmt.Errorf("%s: %q must be one of %s, got %q. An unrecognised value "+
			"is refused rather than defaulted, because a default answers a "+
			"different question than the one the client asked",
			c.ProtocolType(), pp.Name, strings.Join(pp.Enum, ", "), s)
	}
	return nil
}

// handleSchema answers `schema` with the same manifest `iash schema` prints.
// Same function, not a second projection: two documents claiming to be the
// surface is the failure this whole design is arranged to prevent.
func handleSchema(map[string]any) (any, error) {
	return surface.BuildManifest(), nil
}

// handleBlueprintValidate answers `blueprint.validate` with a STRUCTURED result,
// deliberately unlike the CLI's table.
//
// The projection differs because the audience does. A protocol client parsing the
// human output would break the first time a column widened, and a human reading
// this JSON would miss the alignment that makes a differing on_timeout jump out
// of a column. Same capability, same loader, same resolved Config — two
// renderings of it, which is exactly the relationship cmdSurface has to
// cmdSchema.
func handleBlueprintValidate(params map[string]any) (any, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return nil, errors.New("blueprint.validate needs a path to a blueprint file")
	}
	bp, err := blueprint.LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the blueprint is not valid: %w", err)
	}

	c := bp.Config
	type stageOut struct {
		Name        string `json:"name"`
		AdvanceWhen string `json:"advance_when"`
		OnTimeout   string `json:"on_timeout"`
		TimeoutMs   int64  `json:"timeout_ms,omitempty"`
	}
	type memberOut struct {
		Name     string   `json:"name"`
		Tools    []string `json:"tools,omitempty"`
		Advisory bool     `json:"advisory,omitempty"`
	}
	type watcherOut struct {
		Agent   string `json:"agent"`
		Pattern string `json:"pattern"`
		Action  string `json:"action"`
	}

	out := struct {
		Name string `json:"name"`
		SHA  string `json:"sha"`
		// The workspace is reported WITH the reason it resolved that way, the
		// same as the CLI. `worktree` alone invites a client to override it as
		// noise; naming the members that forced it makes the decision reviewable
		// (§20.4).
		Workspace       string       `json:"workspace"`
		WorkspaceReason string       `json:"workspace_reason"`
		Stages          []stageOut   `json:"stages"`
		Members         []memberOut  `json:"members"`
		Watchers        []watcherOut `json:"watchers,omitempty"`
	}{
		Name:            bp.Name,
		SHA:             bp.SHA,
		Workspace:       c.Workspace,
		WorkspaceReason: workspaceReason(c),
		// Non-nil so they marshal as [] rather than null. A client doing
		// `for s in result.stages` should not have to special-case a blueprint
		// with no stages; null and [] mean the same thing here and only one of
		// them is safe to iterate in every language that will read this.
		Stages:  []stageOut{},
		Members: []memberOut{},
	}
	for _, st := range c.Stages {
		out.Stages = append(out.Stages, stageOut{
			Name: st.Name, AdvanceWhen: st.AdvanceWhen,
			OnTimeout: st.OnTimeout, TimeoutMs: st.TimeoutMs,
		})
	}
	for _, m := range c.Members {
		out.Members = append(out.Members, memberOut{
			Name: m.Name, Tools: m.Tools, Advisory: m.Advisory,
		})
	}
	for _, w := range c.Watchers {
		action := w.Action
		if action == "" {
			action = "wake"
		}
		out.Watchers = append(out.Watchers, watcherOut{
			Agent: w.Agent, Pattern: w.Pattern, Action: action,
		})
	}
	return out, nil
}

// cmdServe implements `iash serve [--listen <addr>]`.
func cmdServe(args []string) {
	listen, err := parseServeFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash serve: %v\n\n"+
			"usage: iash serve [--listen unix:///path/to.sock]\n"+
			"       with no --listen it speaks the protocol over stdin/stdout\n", err)
		os.Exit(2)
	}

	if listen == "" {
		// stdio is the default because it needs no cleanup and no permissions
		// decision: the parent process already owns both ends. A socket has a
		// path, a mode and a stale-file problem, and none of that should be forced
		// on the common case of a supervisor spawning one server.
		if err := serveConn(os.Stdin, os.Stdout); err != nil {
			fatal(err)
		}
		return
	}
	serveSocket(strings.TrimPrefix(listen, "unix://"))
}

// serveSocket listens on a unix socket until interrupted.
func serveSocket(path string) {
	// A stale socket file is REFUSED, not removed.
	//
	// Unlinking it silently is the convenient behaviour and it steals the address
	// from a server that is still running: the old process keeps its listener,
	// every new client connects to the new one, and half the clients are talking
	// to a process nobody knows is there. Making the operator remove the file
	// costs one command and cannot do that.
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "iash serve: %s already exists.\n\n"+
			"It is not removed automatically: if another iash is still listening "+
			"there, unlinking the file would leave it running with its listener "+
			"while every new client reached this process instead, so half the "+
			"clients would be talking to a server nobody knows about.\n\n"+
			"  check:  ss -lx | grep %s\n"+
			"  remove: rm %s\n", path, path, path)
		os.Exit(2)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		fatal(fmt.Errorf("listen on %s: %w", path, err))
	}

	// 0700: the filesystem IS the authentication.
	//
	// There is no handshake and no token in this protocol, and a client that can
	// connect can start runs that spend money. Unix permissions are what makes
	// that safe, which is also why a tcp:// address is refused outright in
	// parseServeFlags: a TCP port would offer the same control to the network with
	// nothing in front of it.
	if err := os.Chmod(path, 0o700); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		fatal(fmt.Errorf("restrict %s to its owner: %w", path, err))
	}

	// The socket file is removed on the way out. Because a stale one is refused
	// rather than clobbered, leaving it behind would make the NEXT start fail over
	// a server that is no longer running, and the operator would have to know all
	// of the above to diagnose it.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		_ = ln.Close()
		_ = os.Remove(path)
	}()

	// The banner goes to stderr, not stdout. stdout is the protocol stream in the
	// stdio mode, and a server that greeted the operator on the same channel would
	// make the two modes disagree about what stdout means.
	fmt.Fprintf(os.Stderr, "iash serve: listening on unix://%s (surface v%d)\n",
		path, surface.SurfaceVersion)

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Accept failing means the listener is closed, which is the shutdown
			// path above. Return quietly: a logged error here would make every
			// clean Ctrl-C look like a crash.
			return
		}
		// One goroutine per connection, and requests WITHIN a connection stay
		// ordered (see serveConn). Serialising across connections instead would
		// let one client blocked on a slow validate stall every other client, and
		// the stall would look exactly like the quiescence ADR-0004 is about.
		go func(c net.Conn) {
			defer c.Close()
			if err := serveConn(c, c); err != nil {
				fmt.Fprintf(os.Stderr, "iash serve: connection ended: %v\n", err)
			}
		}(conn)
	}
}

// parseServeFlags reads --listen and refuses the addresses that are unsafe.
func parseServeFlags(args []string) (string, error) {
	listen := ""
	for i := 0; i < len(args); i++ {
		name, val, inline := strings.Cut(args[i], "=")
		switch name {
		case "--listen":
			if inline {
				listen = val
			} else {
				if i+1 >= len(args) {
					return "", errors.New("--listen needs an address")
				}
				i++
				listen = args[i]
			}
		default:
			return "", fmt.Errorf("unknown flag %s", args[i])
		}
	}

	if listen == "" {
		return "", nil
	}

	// tcp:// is refused rather than supported.
	//
	// This protocol has no authentication: a connected client can start runs,
	// inject prompts and spend the budget. On a unix socket the filesystem
	// permissions are the authentication (0700 above). On a TCP port there is
	// nothing, so `--listen tcp://0.0.0.0:9000` would hand full control of the
	// orchestrator to whoever reaches the port. Refusing is not a missing feature;
	// adding it needs auth first, and the message says so.
	//
	// Parenthesised deliberately: && binds tighter than || in Go, so without the
	// parentheses the intent would survive as an accident rather than a
	// statement. The rule is "tcp:// explicitly, or anything carrying a
	// port-looking colon that is not a unix path" — which catches `:9000` and
	// `0.0.0.0:9000`, the two spellings somebody reaches for when unix:// feels
	// like a detour.
	if strings.HasPrefix(listen, "tcp://") ||
		(strings.Contains(listen, ":") && !strings.HasPrefix(listen, "unix://")) {
		return "", fmt.Errorf("--listen %q is not supported: this protocol has no "+
			"authentication, so the unix socket's file permissions ARE the "+
			"authentication. A TCP port would give whoever reaches it the ability "+
			"to start runs and spend the budget. Use unix:///path/to.sock", listen)
	}
	if !strings.HasPrefix(listen, "unix://") {
		return "", fmt.Errorf("--listen %q must be a unix:// address, "+
			"for example unix:///tmp/iash.sock", listen)
	}
	if strings.TrimPrefix(listen, "unix://") == "" {
		return "", errors.New("--listen unix:// has no path")
	}
	return listen, nil
}

// protoHello builds the greeting. It is a function rather than a package-level
// value so the lists are derived from the registry at call time; a variable would
// freeze whatever the registry looked like at init and would keep working after
// somebody changed it.
func protoHello() helloMsg {
	var types, impl []string
	for _, c := range surface.ProtocolCommands() {
		types = append(types, c.ProtocolType())
		if _, ok := protoHandlers[c.ProtocolType()]; ok {
			impl = append(impl, c.ProtocolType())
		}
	}
	// Non-nil so both marshal as [] rather than null, for the same reason as the
	// blueprint result above: a client iterating `implemented` should not have to
	// special-case a build that implements nothing.
	if impl == nil {
		impl = []string{}
	}
	if types == nil {
		types = []string{}
	}
	return helloMsg{
		Type:           "hello",
		Version:        version,
		SurfaceVersion: surface.SurfaceVersion,
		Types:          types,
		Implemented:    impl,
	}
}
