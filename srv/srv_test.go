package srv

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/gravitational/teleport/Godeps/_workspace/src/golang.org/x/crypto/ssh"
	"github.com/gravitational/teleport/Godeps/_workspace/src/golang.org/x/crypto/ssh/agent"
	"github.com/gravitational/teleport/auth"
	"github.com/gravitational/teleport/auth/openssh"
	"github.com/gravitational/teleport/backend"
	"github.com/gravitational/teleport/backend/membk"

	. "github.com/gravitational/teleport/Godeps/_workspace/src/gopkg.in/check.v1"
)

func TestSrv(t *testing.T) { TestingT(t) }

type SrvSuite struct {
	srv *Server
	clt *ssh.Client
	bk  *membk.MemBackend
	a   *auth.AuthServer
	up  *upack
}

var _ = Suite(&SrvSuite{})

func (s *SrvSuite) SetUpSuite(c *C) {
}

func (s *SrvSuite) SetUpTest(c *C) {
	s.bk = membk.New()
	s.a = auth.NewAuthServer(s.bk, openssh.New())

	// set up host private key and certificate
	c.Assert(s.a.ResetHostCA(""), IsNil)
	hpriv, hpub, err := s.a.GenerateKeyPair("")
	c.Assert(err, IsNil)
	hcert, err := s.a.GenerateHostCert(hpub, "localhost", "localhost", 0)
	c.Assert(err, IsNil)

	// set up user CA and set up a user that has access to the server
	c.Assert(s.a.ResetUserCA(""), IsNil)

	cfg := Config{
		Addr:     "localhost:0",
		HostCert: hcert,
		HostKey:  hpriv,
		Backend:  s.bk,
		Shell:    "/bin/sh",
	}
	srv, err := New(cfg)
	c.Assert(err, IsNil)
	s.srv = srv

	c.Assert(s.srv.Start(), IsNil)

	// set up SSH client using the user private key for signing
	up, err := newUpack("test", s.a)
	c.Assert(err, IsNil)

	// set up an agent server and a client that uses agent for forwarding
	keyring := agent.NewKeyring()
	c.Assert(keyring.Add(up.pkey, up.pcert, ""), IsNil)
	s.up = up

	sshConfig := &ssh.ClientConfig{
		User: "test",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(up.certSigner)},
	}
	client, err := ssh.Dial("tcp", s.srv.l.Addr().String(), sshConfig)
	c.Assert(err, IsNil)
	c.Assert(agent.ForwardToAgent(client, keyring), IsNil)
	s.clt = client
}

func (s *SrvSuite) TearDownTest(c *C) {
	c.Assert(s.clt.Close(), IsNil)
	c.Assert(s.srv.Close(), IsNil)
}

// TestExec executes a command on a remote server
func (s *SrvSuite) TestExec(c *C) {
	se, err := s.clt.NewSession()
	c.Assert(err, IsNil)
	defer se.Close()

	out, err := se.Output("expr 2 + 3")
	c.Assert(err, IsNil)
	c.Assert(strings.Trim(string(out), " \n"), Equals, "5")
}

// TestShell launches interactive shell session and executes a command
func (s *SrvSuite) TestShell(c *C) {
	se, err := s.clt.NewSession()
	c.Assert(err, IsNil)

	w, err := se.StdinPipe()
	c.Assert(err, IsNil)

	stdout := &bytes.Buffer{}
	se.Stdout = stdout
	c.Assert(se.Shell(), IsNil)
	_, err = io.WriteString(w, "expr 7 + 70;exit\r\n")
	c.Assert(err, IsNil)
	c.Assert(se.Wait(), IsNil)
	c.Assert(removeNL(stdout.String()), Matches, ".*77.*")
}

// TestMux tests multiplexing command with agent forwarding
func (s *SrvSuite) TestMux(c *C) {
	se, err := s.clt.NewSession()
	c.Assert(err, IsNil)
	defer se.Close()
	c.Assert(agent.RequestAgentForwarding(se), IsNil)

	stdout := &bytes.Buffer{}
	reader, err := se.StdoutPipe()
	done := make(chan struct{})
	go func() {
		io.Copy(stdout, reader)
		close(done)
	}()

	c.Assert(se.RequestSubsystem(fmt.Sprintf("mux:%v/expr 22 + 55", s.srv.l.Addr().String())), IsNil)
	<-done
	c.Assert(removeNL(stdout.String()), Matches, ".*77.*")
}

// TestTun tests tunneling command with agent forwarding
func (s *SrvSuite) TestTun(c *C) {
	se, err := s.clt.NewSession()
	c.Assert(err, IsNil)
	defer se.Close()
	c.Assert(agent.RequestAgentForwarding(se), IsNil)

	writer, err := se.StdinPipe()
	c.Assert(err, IsNil)

	stdout := &bytes.Buffer{}
	reader, err := se.StdoutPipe()
	done := make(chan struct{})
	go func() {
		io.Copy(stdout, reader)
		close(done)
	}()

	c.Assert(se.RequestSubsystem(fmt.Sprintf("tun:%v", s.srv.l.Addr().String())), IsNil)

	_, err = io.WriteString(writer, "expr 7 + 70;exit\r\n")
	c.Assert(err, IsNil)

	<-done
	c.Assert(removeNL(stdout.String()), Matches, ".*77.*")
}

// TestPTY requests PTY for an interactive session
func (s *SrvSuite) TestPTY(c *C) {
	se, err := s.clt.NewSession()
	c.Assert(err, IsNil)
	defer se.Close()

	// request PTY
	c.Assert(se.RequestPty("xterm", 30, 30, ssh.TerminalModes{}), IsNil)
}

// TestEnv requests setting environment variables. (We are currently ignoring these requests)
func (s *SrvSuite) TestEnv(c *C) {
	se, err := s.clt.NewSession()
	c.Assert(err, IsNil)
	defer se.Close()

	c.Assert(se.Setenv("HOME", "/"), IsNil)
}

// TestNoAuth tries to log in with no auth methods and should be rejected
func (s *SrvSuite) TestNoAuth(c *C) {
	_, err := ssh.Dial("tcp", s.srv.l.Addr().String(), &ssh.ClientConfig{})
	c.Assert(err, NotNil)
}

// TestPasswordAuth tries to log in with empty pass and should be rejected
func (s *SrvSuite) TestPasswordAuth(c *C) {
	config := &ssh.ClientConfig{Auth: []ssh.AuthMethod{ssh.Password("")}}
	_, err := ssh.Dial("tcp", s.srv.l.Addr().String(), config)
	c.Assert(err, NotNil)
}

// TODO(klizhentas): figure out the way to check that resources are properly deallocated
// on client disconnects
func (s *SrvSuite) TestClientDisconnect(c *C) {
	config := &ssh.ClientConfig{
		User: "test",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(s.up.certSigner)},
	}
	clt, err := ssh.Dial("tcp", s.srv.l.Addr().String(), config)
	c.Assert(clt, NotNil)
	c.Assert(err, IsNil)

	se, err := s.clt.NewSession()
	c.Assert(err, IsNil)
	c.Assert(se.Shell(), IsNil)
	c.Assert(clt.Close(), IsNil)
}

// upack holds all ssh signing artefacts needed for signing and checking user keys
type upack struct {
	// key is a raw private user key
	key []byte

	// pkey is parsed private SSH key
	pkey interface{}

	// pub is a public user key
	pub []byte

	//cert is a certificate signed by user CA
	cert []byte
	// pcert is a parsed ssh Certificae
	pcert *ssh.Certificate

	// signer is a signer that answers signing challenges using private key
	signer ssh.Signer

	// certSigner is a signer that answers signing challenges using private
	// key and a certificate issued by user certificate authority
	certSigner ssh.Signer
}

func newUpack(user string, a *auth.AuthServer) (*upack, error) {
	upriv, upub, err := a.GenerateKeyPair("")
	if err != nil {
		return nil, err
	}

	ucert, err := a.UpsertUserKey(user, backend.AuthorizedKey{ID: user, Value: upub}, 0)
	if err != nil {
		return nil, err
	}

	upkey, err := ssh.ParseRawPrivateKey(upriv)
	if err != nil {
		return nil, err
	}

	usigner, err := ssh.NewSignerFromKey(upkey)
	if err != nil {
		return nil, err
	}

	pcert, _, _, _, err := ssh.ParseAuthorizedKey(ucert)
	if err != nil {
		return nil, err
	}

	ucertSigner, err := ssh.NewCertSigner(pcert.(*ssh.Certificate), usigner)
	if err != nil {
		return nil, err
	}

	return &upack{
		key:        upriv,
		pkey:       upkey,
		pub:        upub,
		cert:       ucert,
		pcert:      pcert.(*ssh.Certificate),
		signer:     usigner,
		certSigner: ucertSigner,
	}, nil
}

func removeNL(v string) string {
	v = strings.Replace(v, "\r", "", -1)
	v = strings.Replace(v, "\n", "", -1)
	return v
}

// netPipe is analogous to net.Pipe, but it uses a real net.Conn, and
// therefore is buffered (net.Pipe deadlocks if both sides start with
// a write.). This code is courtesy of crypto/ssh/agent/client_test
func netPipe() (net.Conn, net.Conn, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	defer listener.Close()
	c1, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		return nil, nil, err
	}

	c2, err := listener.Accept()
	if err != nil {
		c1.Close()
		return nil, nil, err
	}

	return c1, c2, nil
}
