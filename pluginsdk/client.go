package pluginsdk

import (
	"context"
	"net/http"
)

// HostClient calls the fleet host services. Endpoint is the private host
// listener, Token the plugin's host token and InstanceID the value the plugin
// reports in its manifest. Use NewHTTPClient for HTTP.
type HostClient struct {
	Endpoint, Token, InstanceID string
	HTTP                        *http.Client
}

// Session fetches the current host session. Fleet answers 409 until its
// manifest probe has seen InstanceID, and 403 while the plugin is not enabled
// and ready.
func (c HostClient) Session(ctx context.Context) (Session, error) {
	var session Session
	err := GetJSON(ctx, c.HTTP, c.Endpoint+SessionPath, c.Token, http.Header{InstanceHeader: {c.InstanceID}}, 4096, &session)
	if err == nil && (!ValidUID(session.Epoch) || session.InstanceID != c.InstanceID) {
		err = ErrProtocol
	}
	return session, err
}

// Headers returns the instance and epoch headers every host request except
// the session carries.
func (c HostClient) Headers(session Session) http.Header {
	return http.Header{InstanceHeader: {c.InstanceID}, EpochHeader: {session.Epoch}}
}
