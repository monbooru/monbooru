package peer

import (
	"cmp"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

// A worker pairs as app monworker with its name as the label; its token
// is paired under WorkerKey, one per name, and carries no scope: it lets
// the worker remove its own pairing and nothing else.
func WorkerKey(label string) string { return contract.AppMonworker + ":" + label }

func WorkerLabel(paired string) (string, bool) {
	name, ok := strings.CutPrefix(paired, contract.AppMonworker+":")
	if !ok {
		return "", false
	}
	return name, true
}

func (r Req) Paired() string {
	if r.App == contract.AppMonworker {
		return WorkerKey(r.WorkerName())
	}
	return r.App
}

func (r Req) WorkerName() string { return cmp.Or(r.Label, contract.AppMonworker) }

func grantWorker(c *config.Config, req Req, url string) {
	name := req.WorkerName()
	w := config.WorkerConfig{Name: name, URL: url, Token: req.PeerToken}
	if old := FindWorker(c, name); old != nil {
		w.OnDemand, w.GatewayToken = old.OnDemand, old.GatewayToken
		*old = w
		return
	}
	c.Workers = append(c.Workers, w)
}

func FindWorker(c *config.Config, name string) *config.WorkerConfig {
	i := slices.IndexFunc(c.Workers, func(w config.WorkerConfig) bool { return w.Name == name })
	if i < 0 {
		return nil
	}
	return &c.Workers[i]
}
