package service

import "github.com/QuantumNous/new-api/model"

func ExchangeStudioLogin(code, origin, verifier string, expectedUser int, ip, userAgent string) (*AuthBundle, *model.User, error) {
	session, secret, err := newLoginSession(0, 0, "community-studio", ip, userAgent)
	if err != nil {
		return nil, nil, err
	}
	user, err := model.ExchangeStudioGrant(code, origin, verifier, expectedUser, session)
	if err != nil {
		return nil, nil, err
	}
	bundle, err := issueAuthBundle(session, session.SID+"."+secret, true)
	if err != nil {
		_, _ = model.RevokeUserSession(user.Id, session.SID, "studio_token_failed")
		return nil, nil, err
	}
	return bundle, user, nil
}
