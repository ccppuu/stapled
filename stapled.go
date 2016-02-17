package stapled

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/jmhodges/clock"
)

type stapled struct {
	log                    *Logger
	clk                    clock.Clock
	c                      *cache
	responder              *http.Server
	dontDieOnStaleResponse bool
}

func New(log *Logger, clk clock.Clock, httpAddr string, dontDieOnStale bool, entries []*Entry) (*stapled, error) {
	c := &cache{log, make(map[string]*Entry), make(map[[32]byte]*Entry), new(sync.RWMutex)}
	s := &stapled{log: log, clk: clk, c: c, dontDieOnStaleResponse: dontDieOnStale}
	// add entries to cache
	for _, e := range entries {
		c.add(e)
	}
	// initialize OCSP repsonder
	s.initResponder(httpAddr)
	return s, nil
}

func (s *stapled) Run() error {
	err := s.responder.ListenAndServe()
	if err != nil {
		return fmt.Errorf("HTTP server died: %s", err)
	}
	return nil
}
