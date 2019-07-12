package fakes

import (
	"context"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/remote-apis-sdks/go/digest"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/rexec"
	"github.com/golang/protobuf/ptypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rc "github.com/bazelbuild/remote-apis-sdks/go/client"
	regrpc "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bsgrpc "google.golang.org/genproto/googleapis/bytestream"
)

// Server is a configurable fake in-process RBE server for use in integration tests.
type Server struct {
	Exec        *Exec
	CAS         *CAS
	ActionCache *ActionCache
	listener    net.Listener
	srv         *grpc.Server
}

// NewServer creates a server that is ready to accept requests.
func NewServer() (s *Server, err error) {
	cas := NewCAS()
	ac := NewActionCache()
	s = &Server{Exec: NewExec(ac, cas), CAS: cas, ActionCache: ac}
	s.listener, err = net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	s.srv = grpc.NewServer()
	bsgrpc.RegisterByteStreamServer(s.srv, s.CAS)
	regrpc.RegisterContentAddressableStorageServer(s.srv, s.CAS)
	regrpc.RegisterActionCacheServer(s.srv, s.ActionCache)
	regrpc.RegisterExecutionServer(s.srv, s.Exec)
	go s.srv.Serve(s.listener)
	return s, nil
}

// Clear clears the fake results.
func (s *Server) Clear() {
	s.CAS.Clear()
	s.ActionCache.Clear()
	s.Exec.Clear()
}

// Stop shuts down the in process server.
func (s *Server) Stop() {
	s.listener.Close()
	s.srv.Stop()
}

// NewTestClient returns a new in-process Client connected to this server.
func (s *Server) NewTestClient(ctx context.Context) (*rc.Client, error) {
	return rc.Dial(ctx, "instance", rc.DialParams{
		Service:    s.listener.Addr().String(),
		NoSecurity: true,
	})
}

// TestEnv is a wrapper for convenient integration tests of remote execution.
type TestEnv struct {
	Client     *rexec.Client
	Server     *Server
	ExecRoot   string
	t          *testing.T
}

// NewTestEnv initializes a TestEnv containing a fake server, a client connected to it,
// and a temporary directory used as execution root for inputs and outputs.
// It returns the new env and a cleanup function that should be called in the end of the test.
func NewTestEnv(t *testing.T) (*TestEnv, func()) {
	t.Helper()
	// Set up temp directory.
	execRoot, err := ioutil.TempDir("", strings.ReplaceAll(t.Name(), string(filepath.Separator), "_"))
	if err != nil {
		t.Fatalf("failed to make temp dir: %v", err)
	}
	// Set up the fake.
	s, err := NewServer()
	if err != nil {
		t.Fatalf("Error starting fake server: %v", err)
	}
	grpcClient, err := s.NewTestClient(context.Background())
	if err != nil {
		t.Fatalf("Error connecting to server: %v", err)
	}
	return &TestEnv{
		Client:     &rexec.Client{&rexec.NoopFileDigestCache{}, grpcClient},
		Server:     s,
		ExecRoot:   execRoot,
		t:          t,
	}, func(){
		grpcClient.Close()
		s.Stop()
		os.RemoveAll(execRoot)
	}
}

// Set sets up the fake to return the given result on the given command execution.
// It is not possible to make the fake result in a LocalErrorResultStatus or an InterruptedResultStatus.
func (e *TestEnv) Set(cmd *command.Command, opt *command.ExecutionOptions, res *command.Result, opts ...option) (cmdDg, acDg digest.Digest) {
	e.t.Helper()
	cmd.FillDefaultFieldValues()
	ft, err := rc.BuildTreeFromInputs(cmd.ExecRoot, cmd.InputSpec)
	if err != nil {
		e.t.Fatalf("error building input tree in fake setup: %v", err)
		return digest.Empty, digest.Empty
	}
	root, _, err := rc.PackageTree(ft)
	if err != nil {
		e.t.Fatalf("error building input tree in fake setup: %v", err)
		return digest.Empty, digest.Empty
	}

	cmdPb := cmd.ToREProto()
	cmdDg = digest.TestNewFromMessage(cmdPb)
	ac := &repb.Action{
		CommandDigest:   cmdDg.ToProto(),
		InputRootDigest: root.ToProto(),
		DoNotCache:      opt.DoNotCache,
	}
	if cmd.Timeout > 0 {
		ac.Timeout = ptypes.DurationProto(cmd.Timeout)
	}
	ar := &repb.ActionResult{
		ExitCode: int32(res.ExitCode),
	}
	acDg = digest.TestNewFromMessage(ac)
	for _, o := range opts {
		o.Apply(ar, e.Server)
	}
	e.Server.Exec.ActionResult = ar
	switch res.Status {
	case command.TimeoutResultStatus:
		e.Server.Exec.Status = status.New(codes.DeadlineExceeded, "timeout")
	case command.RemoteErrorResultStatus:
		st, ok := status.FromError(res.Err)
		if !ok {
			st = status.New(codes.Internal, "remote error")
		}
		e.Server.Exec.Status = st
	case command.CacheHitResultStatus:
		if !e.Server.Exec.Cached { // Assume the user means in this case the actual ActionCache should miss.
			e.Server.ActionCache.Put(acDg, ar)
		}
	}
	return cmdDg, acDg
}

type option interface {
	Apply(*repb.ActionResult, *Server)
}

// OutputFile is to be added as an output of the fake action.
type OutputFile struct {
	Path     string
	Contents string
}

// Apply puts the file in the fake CAS and the given ActionResult.
func (f *OutputFile) Apply(ac *repb.ActionResult, s *Server) {
	bytes := []byte(f.Contents)
	s.Exec.OutputBlobs = append(s.Exec.OutputBlobs, bytes)
	dg := s.CAS.Put(bytes)
	ac.OutputFiles = append(ac.OutputFiles, &repb.OutputFile{Path: f.Path, Digest: dg.ToProto()})
}

// StdOut is to be added as an output of the fake action.
type StdOut string

// Apply puts the action stdout in the fake CAS and the given ActionResult.
func (o StdOut) Apply(ac *repb.ActionResult, s *Server) {
	bytes := []byte(o)
	s.Exec.OutputBlobs = append(s.Exec.OutputBlobs, bytes)
	dg := s.CAS.Put(bytes)
	ac.StdoutDigest = dg.ToProto()
}

// StdOutRaw is to be added as a raw output of the fake action.
type StdOutRaw string

// Apply puts the action stdout as raw bytes in the given ActionResult.
func (o StdOutRaw) Apply(ac *repb.ActionResult, s *Server) {
	ac.StdoutRaw = []byte(o)
}

// StdErr is to be added as an output of the fake action.
type StdErr string

// Apply puts the action stderr in the fake CAS and the given ActionResult.
func (o StdErr) Apply(ac *repb.ActionResult, s *Server) {
	bytes := []byte(o)
	s.Exec.OutputBlobs = append(s.Exec.OutputBlobs, bytes)
	dg := s.CAS.Put(bytes)
	ac.StderrDigest = dg.ToProto()
}

// StdErrRaw is to be added as a raw output of the fake action.
type StdErrRaw string

// Apply puts the action stderr as raw bytes in the given ActionResult.
func (o StdErrRaw) Apply(ac *repb.ActionResult, s *Server) {
	ac.StderrRaw = []byte(o)
}

// ExecutionCacheHit of true will cause the ActionResult to be returned as a cache hit during
// fake execution.
type ExecutionCacheHit bool

// Apply on true will cause the ActionResult to be returned as a cache hit during fake execution.
func (c ExecutionCacheHit) Apply(ac *repb.ActionResult, s *Server) {
	s.Exec.Cached = bool(c)
}