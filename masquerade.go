package domainfronted

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

const (
	NumWorkers     = 10 // number of worker goroutines for verifying
	MaxMasquerades = 20 // cap number of verified masquerades at this
)

// Masquerade contains the data for a single masquerade host, including
// the domain and the root CA.
type Masquerade struct {
	// Domain: the domain to use for domain fronting
	Domain string

	// IpAddress: pre-resolved ip address to use instead of Domain (if
	// available)
	IpAddress string
}

type MasqueradeSet []*Masquerade

// verifiedMasqueradeSet represents a set of Masquerade configurations.
// verifiedMasqueradeSet verifies each configured Masquerade by attempting to
// proxy using it.
type verifiedMasqueradeSet struct {
	client             *Client
	candidatesCh       chan *Masquerade
	stopCh             chan interface{}
	verifiedCh         chan *Masquerade
	verifiedCount      int
	verifiedCountMutex sync.Mutex
	wg                 sync.WaitGroup
}

// nextVerified returns the next available verified *Masquerade, blocking until
// such is available.  The masquerade is immediately written back onto
// verifiedCh, turning verifiedCh into a sort of cyclic queue.
func (vms *verifiedMasqueradeSet) nextVerified() *Masquerade {
	masquerade := <-vms.verifiedCh
	vms.verifiedCh <- masquerade
	return masquerade
}

// verified sets up a new verifiedMasqueradeSet that verifies each of the
// Masquerades in this MasqueradeSet for the given client.
func (client *Client) verifiedMasquerades() *verifiedMasqueradeSet {
	// Size verifiedChSize to be able to hold the smaller of MaxMasquerades or
	// the number of configured masquerades.
	verifiedChSize := len(client.cfg.Masquerades)
	if MaxMasquerades < verifiedChSize {
		verifiedChSize = MaxMasquerades
	}
	vms := &verifiedMasqueradeSet{
		client:       client,
		candidatesCh: make(chan *Masquerade),
		stopCh:       make(chan interface{}, 1),
		verifiedCh:   make(chan *Masquerade, verifiedChSize),
	}

	vms.wg.Add(NumWorkers)
	// Spawn some worker goroutines to verify masquerades
	for i := 0; i < NumWorkers; i++ {
		go vms.verify()
	}

	// Feed candidates for verification
	go vms.feedCandidates()

	return vms
}

// feedCandidates feeds the candidate masquerades to our worker routines in
// random order
func (vms *verifiedMasqueradeSet) feedCandidates() {
	for _, i := range rand.Perm(len(vms.client.cfg.Masquerades)) {
		if !vms.feedCandidate(vms.client.cfg.Masquerades[i]) {
			break
		}
	}
	close(vms.candidatesCh)
}

func (vms *verifiedMasqueradeSet) feedCandidate(candidate *Masquerade) bool {
	select {
	case <-vms.stopCh:
		log.Debug("Received stop, not feeding any further")
		return false
	case vms.candidatesCh <- candidate:
		log.Debug("Fed candidate")
		return true
	}
}

// stop stops the verification process
func (vms *verifiedMasqueradeSet) stop() {
	log.Debug("Stop called")
	vms.stopCh <- nil
	log.Debug("Waiting for workers to finish")
	vms.wg.Wait()
	log.Debug("Stopped")
}

// verify checks masquerades obtained from candidatesCh to see if they work on
// the test server.
func (vms *verifiedMasqueradeSet) verify() {
	for {
		candidate, ok := <-vms.candidatesCh
		if !ok {
			vms.wg.Done()
			return
		}
		if !vms.doVerify(candidate) {
			return
		}
	}
}

// doVerify does the verification and returns a boolean indicating whether or
// not to continue processing more verifications.
func (vms *verifiedMasqueradeSet) doVerify(masquerade *Masquerade) bool {
	errCh := make(chan error, 2)
	go func() {
		// Limit amount of time we'll wait for a response
		time.Sleep(30 * time.Second)
		errCh <- fmt.Errorf("Timed out verifying %s", masquerade.Domain)
	}()
	go func() {
		start := time.Now()
		httpClient := vms.client.HttpClientUsing(masquerade)
		req, _ := http.NewRequest("HEAD", "http://www.google.com/humans.txt", nil)
		resp, err := httpClient.Do(req)
		if err != nil {
			errCh <- fmt.Errorf("HTTP ERROR FOR MASQUERADE %v: %v", masquerade.Domain, err)
			return
		} else {
			body, err := ioutil.ReadAll(resp.Body)
			defer resp.Body.Close()
			if err != nil {
				errCh <- fmt.Errorf("HTTP Body Error: %s", body)
			} else {
				delta := time.Now().Sub(start)
				log.Debugf("SUCCESSFUL CHECK FOR: %s IN %s, %s", masquerade.Domain, delta, body)
				errCh <- nil
			}
		}
	}()
	err := <-errCh
	if err != nil {
		log.Error(err)
		return true
	}
	if vms.incrementVerifiedCount() {
		vms.verifiedCh <- masquerade
		return true
	}
	return false
}

// incrementVerifiedCount keeps track of the number of verified masquerades and
// caps it at MaxMasquerades.
func (vms *verifiedMasqueradeSet) incrementVerifiedCount() bool {
	vms.verifiedCountMutex.Lock()
	defer vms.verifiedCountMutex.Unlock()
	if vms.verifiedCount == MaxMasquerades {
		return false
	}
	vms.verifiedCount += 1
	return true
}
