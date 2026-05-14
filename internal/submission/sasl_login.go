package submission

import (
	"errors"

	"github.com/emersion/go-sasl"
)

// newLoginServer builds a sasl.Server implementing the SMTP AUTH LOGIN
// mechanism (legacy, not RFC-standardised but widely deployed for Outlook
// and some Android MUAs). go-sasl ships a LOGIN client but no server, so
// we implement the small state machine here.
//
// Wire:
//
//	S: 334 VXNlcm5hbWU6     ("Username:")
//	C: <base64 username>
//	S: 334 UGFzc3dvcmQ6     ("Password:")
//	C: <base64 password>
//	S: 235 OK / 535 fail
func newLoginServer(authenticate func(username, password string) error) sasl.Server {
	return &loginServer{auth: authenticate}
}

type loginServer struct {
	auth     func(username, password string) error
	state    int
	username string
}

// State machine:
//
//	0 → initial. If go-sasl client uses SASL-IR, response carries the
//	    username already; jump straight to Password: challenge. Otherwise
//	    response is empty and we prompt with Username: first.
//	1 → username challenge was sent; response is the username.
//	2 → password challenge was sent; response is the password → authenticate.
func (s *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch s.state {
	case 0:
		if len(response) > 0 {
			s.username = string(response)
			s.state = 2
			return []byte("Password:"), false, nil
		}
		s.state = 1
		return []byte("Username:"), false, nil
	case 1:
		if response == nil {
			return nil, true, errors.New("sasl/login: missing username")
		}
		s.username = string(response)
		s.state = 2
		return []byte("Password:"), false, nil
	case 2:
		if response == nil {
			return nil, true, errors.New("sasl/login: missing password")
		}
		err = s.auth(s.username, string(response))
		return nil, true, err
	}
	return nil, true, errors.New("sasl/login: unexpected state")
}
