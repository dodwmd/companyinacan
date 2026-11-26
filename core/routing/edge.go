package routing

// Edge limits (specs/045-public-access): Traefik middlewares in front of sign-in and anonymous
// routes. Authenticated API traffic is never limited here: the modules limit that per credential.
//
// Both count per TCP source address as Traefik sees it (ipStrategy depth 0). Request headers such
// as X-Forwarded-For are never read, so a client cannot choose its bucket. k3s is configured to
// keep the client's address (externalTrafficPolicy: Local, deploy/base/traefik-config.yaml).
// Behind a proxy or CDN every client shares that proxy's addresses: switch the limits off there.

// Limit is one edge limit: a request rate with a burst, and a cap on requests in flight.
type Limit struct {
	Average, Burst, InFlight int
	Period                   string
}

var (
	// LoginLimit is for sign-in paths: an office behind one address signing in at once fits in
	// the burst; password guessing from one address is held to one try a second.
	LoginLimit = Limit{Average: 1, Period: "1s", Burst: 30, InFlight: 20}
	// PublicLimit is for anonymous pages, forms and signed links.
	PublicLimit = Limit{Average: 20, Period: "1s", Burst: 100, InFlight: 40}
)

// PublicLimited are the modules whose chart-made public routes get PublicLimit. Others are not
// limited: provisioning's SCIM endpoint is authenticated API traffic, and wrapped apps (analytics
// collection, chat widgets) have traffic patterns this cannot guess.
var PublicLimited = map[string]bool{"jobs": true}

func bySource() map[string]any { return map[string]any{"ipStrategy": map[string]any{"depth": 0}} }

// RateSpec is the spec of the Traefik rateLimit middleware.
func (l Limit) RateSpec() map[string]any {
	return map[string]any{"rateLimit": map[string]any{"average": l.Average, "period": l.Period, "burst": l.Burst, "sourceCriterion": bySource()}}
}

// InFlightSpec is the spec of the Traefik inFlightReq middleware. Its default source is the Host
// header (one bucket for everybody), so the source address is always named.
func (l Limit) InFlightSpec() map[string]any {
	return map[string]any{"inFlightReq": map[string]any{"amount": l.InFlight, "sourceCriterion": bySource()}}
}
