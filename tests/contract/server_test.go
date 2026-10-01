package contract

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const contractSecret = "contract-test-secret"

type serverOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *serverOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *serverOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

type apiServer struct {
	base   string
	cmd    *exec.Cmd
	out    *serverOutput
	exited chan struct{}
	err    error
	client *http.Client
}

// buildAPI builds cmd/api, or returns CONTRACT_API_BINARY when set.
func buildAPI(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("CONTRACT_API_BINARY"); b != "" {
		return b
	}
	bin := filepath.Join(t.TempDir(), "api")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/api")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/api: %v\n%s", err, out)
	}
	return bin
}

// firebaseCredentials is a well-formed service account whose token endpoint is a closed local
// port, so a push attempt fails at once and nothing leaves the machine.
func firebaseCredentials(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "fk-contract", "private_key_id": "k1",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "contract@fk-contract.iam.gserviceaccount.com", "client_id": "1",
		"token_uri": "http://127.0.0.1:1/token",
	})
	return string(b)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startAPI runs the real server against dsn and stops it with SIGTERM when the test ends,
// failing the test unless it shuts down cleanly.
func startAPI(t *testing.T, dsn string) *apiServer {
	t.Helper()
	port := freePort(t)
	srv := &apiServer{
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		out:    &serverOutput{},
		exited: make(chan struct{}),
		client: &http.Client{Timeout: 30 * time.Second},
	}
	srv.cmd = exec.Command(buildAPI(t))
	srv.cmd.Dir = t.TempDir()
	srv.cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"DATABASE_URL=" + dsn, "JWT_SECRET=" + contractSecret, fmt.Sprintf("PORT=%d", port),
		"FIREBASE_CREDENTIALS_JSON=" + firebaseCredentials(t),
		"ABSENCE_CRON_SCHEDULE=0 0 1 1 *",
	}
	srv.cmd.Stdout, srv.cmd.Stderr = srv.out, srv.out
	if err := srv.cmd.Start(); err != nil {
		t.Fatalf("start api: %v", err)
	}
	go func() { srv.err = srv.cmd.Wait(); close(srv.exited) }()
	t.Cleanup(func() {
		select {
		case <-srv.exited:
			t.Errorf("server exited before the test ended: %v\n%s", srv.err, srv.out)
			return
		default:
		}
		srv.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-srv.exited:
			if srv.err != nil {
				t.Errorf("server did not shut down cleanly: %v\n%s", srv.err, srv.out)
			}
		case <-time.After(40 * time.Second):
			srv.cmd.Process.Kill()
			<-srv.exited
			t.Errorf("server ignored SIGTERM for 40s and was killed\n%s", srv.out)
		}
	})

	deadline := time.Now().Add(20 * time.Second)
	for {
		if resp, err := srv.client.Get(srv.base + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return srv
			}
		}
		select {
		case <-srv.exited:
			t.Fatalf("server exited during startup: %v\n%s", srv.err, srv.out)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became healthy:\n%s", srv.out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *apiServer) send(method, target string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequest(method, s.base+target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b, err
}

// signToken signs claims with the server's secret (or another key) the way internal/auth does.
func signToken(t *testing.T, claims jwt.MapClaims, secret string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func unsignedToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claims(role string, parentID any, exp time.Time) jwt.MapClaims {
	c := jwt.MapClaims{"exp": exp.Unix(), "iat": time.Now().Add(-time.Minute).Unix()}
	if role != "" {
		c["role"] = role
		c["sv"] = 0
	}
	if parentID != nil {
		c["parent_id"] = parentID
	}
	if role == "admin" {
		c["username"] = "admin"
	}
	return c
}
