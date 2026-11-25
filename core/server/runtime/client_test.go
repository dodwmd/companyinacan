package runtime

import "net/http"

// noKeepAlive: idle keep-alive connections of the shared default client can be mid-handshake when
// the server shuts down, which makes Shutdown wait out its whole timeout (seen under -race).
var noKeepAlive = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
