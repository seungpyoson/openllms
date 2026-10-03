package proxy

import (
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestEngineConcurrentInitialization(t *testing.T) {
	eng := &Engine{}
	start := make(chan struct{})
	var wg sync.WaitGroup
	account := uuid.New()
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			eng.affinity()
			eng.selector()
			eng.oauthLock(account)
		}()
	}
	close(start)
	wg.Wait()
}
