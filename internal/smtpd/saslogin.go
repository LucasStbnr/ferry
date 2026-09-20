package smtpd

import (
	"errors"

	"github.com/emersion/go-sasl"
)

// AUTH LOGIN is not in any RFC and go-sasl ships only a client for it, but
// Apple Mail still offers it for SMTP submission, so Ferry has to answer it.
// The exchange is two base64 prompts, "Username:" then "Password:", which the
// SMTP layer encodes for us.
//
// LOGIN sends the password in the clear, exactly as PLAIN does. That is
// acceptable here only because the connection is TLS from the first byte.

var errLoginProtocol = errors.New("sasl: unexpected LOGIN exchange")

// LoginAuthenticator verifies a username and password.
type LoginAuthenticator func(username, password string) error

type loginServer struct {
	authenticate LoginAuthenticator
	username     string
	state        int
}

// newLoginServer returns a SASL server for the LOGIN mechanism.
func newLoginServer(auth LoginAuthenticator) sasl.Server {
	return &loginServer{authenticate: auth}
}

// Next implements sasl.Server.
func (s *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch s.state {
	case 0:
		s.state++
		if response == nil {
			return []byte("Username:"), false, nil
		}
		// Some clients send the username as the initial response.
		s.username = string(response)
		return []byte("Password:"), false, nil
	case 1:
		s.state++
		if s.username == "" {
			s.username = string(response)
			return []byte("Password:"), false, nil
		}
		return nil, true, s.authenticate(s.username, string(response))
	case 2:
		s.state++
		return nil, true, s.authenticate(s.username, string(response))
	}
	return nil, false, errLoginProtocol
}
