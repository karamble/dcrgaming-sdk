package bridgetest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
)

// Server is a fake bridge listening on loopback.
//
// It speaks the real thing's protocol over real mTLS on a real socket, because
// a game's transport is one of the parts being tested and stubbing it out would
// leave the interesting failures untested. Each seat gets its own certificate,
// which is how the bridge tells them apart - the same way the real one does.
type Server struct {
	// Addr is host:port.
	Addr string

	bridge     *Bridge
	srv        *grpc.Server
	serverCert []byte

	mu    sync.Mutex
	seats map[string]credPair
	conns []*transport.Bridge
}

type credPair struct{ cert, key []byte }

// Serve stands the bridge up on a loopback port and issues a certificate for
// each named seat. Close it when the test is done.
func (b *Bridge) Serve(seats ...string) (*Server, error) {
	if len(seats) == 0 {
		return nil, fmt.Errorf("a bridge with no seats has nobody to talk to")
	}
	serverCert, serverKey, err := selfSigned("bridge")
	if err != nil {
		return nil, fmt.Errorf("bridge certificate: %w", err)
	}
	pair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf("bridge key pair: %w", err)
	}

	pool := x509.NewCertPool()
	creds := map[string]credPair{}
	for _, name := range seats {
		cert, key, err := selfSigned(name)
		if err != nil {
			return nil, fmt.Errorf("certificate for %q: %w", name, err)
		}
		if !pool.AppendCertsFromPEM(cert) {
			return nil, fmt.Errorf("certificate for %q did not parse", name)
		}
		creds[name] = credPair{cert: cert, key: key}
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{pair},
			MinVersion:   tls.VersionTLS12,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
		})),
		grpc.UnaryInterceptor(b.unreachableUnary),
		grpc.StreamInterceptor(b.unreachableStream),
	)
	gamingpb.RegisterBridgeServiceServer(srv, b)
	go func() { _ = srv.Serve(lis) }()

	return &Server{
		Addr: lis.Addr().String(), bridge: b, srv: srv,
		serverCert: serverCert, seats: creds,
	}, nil
}

// Config is the connection a seat would be given by an operator who had copied
// the credentials across by hand.
//
// Stamp the game identity onto it the way the game does in production, rather
// than having this package guess: what a game calls itself is the game's own
// business, and a test that skipped the stamping would not exercise it.
func (s *Server) Config(seat string) (transport.BridgeConfig, error) {
	s.mu.Lock()
	c, ok := s.seats[seat]
	s.mu.Unlock()
	if !ok {
		return transport.BridgeConfig{}, fmt.Errorf("no seat named %q was issued a certificate", seat)
	}
	return transport.BridgeConfig{
		Addr:       s.Addr,
		ClientCert: c.cert,
		ClientKey:  c.key,
		BridgeCert: s.serverCert,
	}, nil
}

// Dial connects one seat. The stamp is the game's own identity function, the
// one it uses in production; pass nil only if the test does not care.
func (s *Server) Dial(ctx context.Context, seat string, stamp func(*transport.BridgeConfig)) (*transport.Bridge, error) {
	cfg, err := s.Config(seat)
	if err != nil {
		return nil, err
	}
	if stamp != nil {
		stamp(&cfg)
	}
	conn, err := transport.Dial(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("dial as %q: %w", seat, err)
	}
	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.mu.Unlock()
	return conn, nil
}

// Close stops the server and every connection it handed out.
func (s *Server) Close() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	s.srv.Stop()
}

// selfSigned issues a throwaway certificate with the name as its common name,
// which is the identity the bridge routes on.
func selfSigned(name string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
