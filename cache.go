package main

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/ioutil"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jmhodges/clock"
	"golang.org/x/crypto/ocsp"
	"golang.org/x/net/context"
)

type cache struct {
	log       *Logger
	entries   map[string]*Entry   // one-to-one map keyed on name -> entry
	lookupMap map[[32]byte]*Entry // many-to-one map keyed on sha256 hashed OCSP requests -> entry
	mu        sync.RWMutex
}

func newCache(log *Logger, monitorTick time.Duration) *cache {
	c := &cache{
		log:       log,
		entries:   make(map[string]*Entry),
		lookupMap: make(map[[32]byte]*Entry),
	}
	go c.monitor(monitorTick)
	return c
}

func hashEntry(h hash.Hash, name, pkiBytes []byte, serial *big.Int) ([32]byte, error) {
	issuerNameHash, issuerKeyHash, err := hashNameAndPKI(h, name, pkiBytes)
	if err != nil {
		return [32]byte{}, err
	}
	serialHash := sha256.Sum256(serial.Bytes())
	return sha256.Sum256(append(append(issuerNameHash, issuerKeyHash...), serialHash[:]...)), nil
}

func allHashes(e *Entry) ([][32]byte, error) {
	results := [][32]byte{}
	// these should be configurable in case people don't care about
	// supporting all of these hash algs
	for _, h := range []crypto.Hash{crypto.SHA1, crypto.SHA256, crypto.SHA384, crypto.SHA512} {
		hashed, err := hashEntry(h.New(), e.issuer.RawSubject, e.issuer.RawSubjectPublicKeyInfo, e.serial)
		if err != nil {
			return nil, err
		}
		results = append(results, hashed)
	}
	return results, nil
}

func hashRequest(request *ocsp.Request) [32]byte {
	serialHash := sha256.Sum256(request.SerialNumber.Bytes())
	return sha256.Sum256(append(append(request.IssuerNameHash, request.IssuerKeyHash...), serialHash[:]...))
}

func (c *cache) lookup(request *ocsp.Request) (*Entry, bool) {
	hash := hashRequest(request)
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, present := c.lookupMap[hash]
	return e, present
}

func (c *cache) lookupResponse(request *ocsp.Request) ([]byte, bool) {
	e, present := c.lookup(request)
	if present {
		e.mu.RLock()
		defer e.mu.RUnlock()
		return e.response, present
	}
	return nil, present
}

func (c *cache) addSingle(e *Entry, key [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, present := c.entries[e.name]; present {
		c.log.Warning("[cache] Entry for '%s' already exists in cache", e.name)
		return
	}
	c.log.Info("[cache] Adding entry for '%s'", e.name)
	c.entries[e.name] = e
	c.lookupMap[key] = e
}

// this cache structure seems kind of gross but... idk i think it's prob
// best for now (until I can think of something better :/)
func (c *cache) addMulti(e *Entry) error {
	hashes, err := allHashes(e)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, present := c.entries[e.name]; present {
		// log or fail...?
		c.log.Warning("[cache] Overwriting cache entry '%s'", e.name)
	} else {
		c.log.Info("[cache] Adding entry for '%s'", e.name)
	}
	c.entries[e.name] = e
	for _, h := range hashes {
		c.lookupMap[h] = e
	}
	return nil
}

func (c *cache) remove(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, present := c.entries[name]
	if !present {
		return fmt.Errorf("entry '%s' is not in the cache", name)
	}
	e.mu.Lock()
	delete(c.entries, name)
	hashes, err := allHashes(e)
	if err != nil {
		return err
	}
	for _, h := range hashes {
		delete(c.lookupMap, h)
	}
	c.log.Info("[cache] Removed entry for '%s' from cache", name)
	return nil
}

func (c *cache) monitor(tick time.Duration) {
	ticker := time.NewTicker(tick)
	for range ticker.C {
		c.mu.RLock()
		defer c.mu.RUnlock()
		for _, entry := range c.entries {
			go entry.refreshAndLog()
		}
	}
}

type Entry struct {
	name     string
	log      *Logger
	clk      clock.Clock
	lastSync time.Time

	// cert related
	serial *big.Int
	issuer *x509.Certificate

	// request related
	responders  []string
	client      *http.Client
	timeout     time.Duration
	baseBackoff time.Duration
	request     []byte

	// response related
	maxAge           time.Duration
	eTag             string
	response         []byte
	responseFilename string
	nextUpdate       time.Time
	thisUpdate       time.Time

	mu *sync.RWMutex
}

func NewEntry(log *Logger, clk clock.Clock, timeout, baseBackoff time.Duration) *Entry {
	return &Entry{
		log:         log,
		clk:         clk,
		client:      new(http.Client),
		timeout:     timeout,
		baseBackoff: baseBackoff,
		mu:          new(sync.RWMutex),
	}
}

func loadProxy(uri string) (func(*http.Request) (*url.URL, error), error) {
	proxyURL, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("failed to parse proxy URL: %s", err)
	}
	return http.ProxyURL(proxyURL), nil
}

func (e *Entry) generateResponseFilename(cacheFolder string) {
	e.responseFilename = path.Join(
		cacheFolder,
		fmt.Sprintf(
			"%s.resp",
			strings.TrimSuffix(
				filepath.Base(e.name),
				filepath.Ext(e.name),
			),
		),
	)
}

func (e *Entry) loadCertificate(filename string) error {
	e.name = filename
	cert, err := ReadCertificate(filename)
	if err != nil {
		return err
	}
	e.serial = cert.SerialNumber
	e.responders = cert.OCSPServer
	if e.issuer == nil && len(cert.IssuingCertificateURL) > 0 {
		for _, issuerURL := range cert.IssuingCertificateURL {
			// this should be its own function
			resp, err := http.Get(issuerURL)
			if err != nil {
				e.log.Err("Failed to retrieve issuer from '%s': %s", issuerURL, err)
				continue
			}
			defer resp.Body.Close()
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				e.log.Err("Failed to read issuer body from '%s': %s", issuerURL, err)
				continue
			}
			e.issuer, err = ParseCertificate(body)
			if err != nil {
				e.log.Err("Failed to parse issuer body from '%s': %s", issuerURL, err)
				continue
			}
		}
	}
	return nil
}

func (e *Entry) loadCertificateInfo(name, serial string) error {
	e.name = name
	e.responseFilename = name + ".resp"
	serialBytes, err := hex.DecodeString(serial)
	if err != nil {
		return fmt.Errorf("failed to decode serial '%s': %s", e.serial, err)
	}
	e.serial = e.serial.SetBytes(serialBytes)
	return nil
}

// blergh
func (e *Entry) FromCertDef(def CertDefinition, globalUpstream []string, globalProxy string, cacheFolder string) error {
	if def.Issuer != "" {
		var err error
		e.issuer, err = ReadCertificate(def.Issuer)
		if err != nil {
			return err
		}
	}
	if def.Certificate != "" {
		err := e.loadCertificate(def.Certificate)
		if err != nil {
			return err
		}
	} else if def.Name != "" && def.Serial != "" {
		err := e.loadCertificateInfo(def.Name, def.Serial)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("either certificate or name and serial must be provided")
	}
	if e.issuer == nil {
		return fmt.Errorf("either issuer or a certificate containing issuer AIA information must be provided")
	}
	if cacheFolder != "" {
		e.generateResponseFilename(cacheFolder)
	}
	if len(globalUpstream) > 0 && !def.OverrideGlobalUpstream {
		e.responders = globalUpstream
	} else if len(def.Responders) > 0 {
		e.responders = def.Responders
	}
	proxyURI := ""
	if globalProxy != "" && !def.OverrideGlobalProxy {
		proxyURI = globalProxy
	} else if def.Proxy != "" {
		proxyURI = def.Proxy
	}
	if proxyURI != "" {
		proxy, err := loadProxy(proxyURI)
		if err != nil {
			return err
		}
		e.client.Transport = &http.Transport{
			Proxy: proxy,
			Dial: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout: 10 * time.Second,
		}
	}
	return nil
}

func (e *Entry) Init() error {
	if e.request == nil {
		if e.issuer == nil {
			return errors.New("if request isn't provided issuer must be non-nil")
		}
		issuerNameHash, issuerKeyHash, err := hashNameAndPKI(
			crypto.SHA1.New(),
			e.issuer.RawSubject,
			e.issuer.RawSubjectPublicKeyInfo,
		)
		if err != nil {
			return err
		}
		ocspRequest := &ocsp.Request{
			crypto.SHA1,
			issuerNameHash,
			issuerKeyHash,
			e.serial,
		}
		e.request, err = ocspRequest.Marshal()
		if err != nil {
			return err
		}
	}
	for i := range e.responders {
		e.responders[i] = strings.TrimSuffix(e.responders[i], "/")
	}
	err := e.readFromDisk()
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		e.err("Failed to read response from disk: %s", err)
	}
	err = e.refreshResponse()
	if err != nil {
		return err
	}

	return nil
}

// info makes a Info Logger call tagged with the entry name
func (e *Entry) info(msg string, args ...interface{}) {
	e.log.Info(fmt.Sprintf("[entry:%s] %s", e.name, msg), args...)
}

// info makes a Err Logger call tagged with the entry name
func (e *Entry) err(msg string, args ...interface{}) {
	e.log.Err(fmt.Sprintf("[entry:%s] %s", e.name, msg), args...)
}

// writeToDisk writes a response to disk. Assumes the
// caller holds a write lock
func (e *Entry) writeToDisk() error {
	tmpName := fmt.Sprintf("%s.tmp", e.responseFilename)
	err := ioutil.WriteFile(tmpName, e.response, os.ModePerm)
	if err != nil {
		return err
	}
	err = os.Rename(tmpName, e.responseFilename)
	if err != nil {
		return err
	}
	e.info("Written new response to %s", e.responseFilename)
	return nil
}

// readFromDisk attempts to read a response that has been
// cached on disk
func (e *Entry) readFromDisk() error {
	respBytes, err := ioutil.ReadFile(e.responseFilename)
	if err != nil {
		return err
	}
	e.info("Read response from %s", e.responseFilename)
	resp, err := ocsp.ParseResponse(respBytes, e.issuer)
	if err != nil {
		return err
	}
	err = e.verifyResponse(resp)
	if err != nil {
		return err
	}
	e.updateResponse("", 0, resp, respBytes, false)
	return nil
}

// updateResponse updates the actual response body/metadata
// stored in the entry
func (e *Entry) updateResponse(eTag string, maxAge int, resp *ocsp.Response, respBytes []byte, write bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.eTag = eTag
	e.maxAge = time.Second * time.Duration(maxAge)
	e.lastSync = e.clk.Now()
	if resp != nil {
		e.response = respBytes
		e.nextUpdate = resp.NextUpdate
		e.thisUpdate = resp.ThisUpdate
		if e.responseFilename != "" && write {
			err := e.writeToDisk()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// refreshResponse fetches and verifies a response and replaces
// the current response if it is valid and newer
func (e *Entry) refreshResponse() error {
	if !e.timeToUpdate() {
		return nil
	}
	responder := randomResponder(e.responders)
	e.info("Fetching response from %s", responder)
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()
	resp, respBytes, eTag, maxAge, err := e.fetchResponse(ctx, responder)
	if err != nil {
		return err
	}

	e.mu.RLock()
	if resp == nil || bytes.Compare(respBytes, e.response) == 0 {
		e.mu.RUnlock()
		e.info("Response hasn't changed since last sync")
		e.updateResponse(eTag, maxAge, nil, nil, true)
		return nil
	}
	e.mu.RUnlock()
	err = e.verifyResponse(resp)
	if err != nil {
		return err
	}
	e.updateResponse(eTag, maxAge, resp, respBytes, true)
	e.info("Response has been refreshed")
	return nil
}

// refreshAndLog is a small wrapper around refreshResponse
// for when a caller wants to run it in a goroutine and doesn't
// want to handle the returned error itself
func (e *Entry) refreshAndLog() {
	err := e.refreshResponse()
	if err != nil {
		e.err("Failed to refresh response", err)
	}
}

// timeToUpdate checks if a current entry should be refreshed
// because cache parameters expired or it is in it's update window
func (e *Entry) timeToUpdate() bool {
	now := e.clk.Now()
	e.mu.RLock()
	defer e.mu.RUnlock()
	// no response or nextUpdate is in the past
	if e.response == nil || e.nextUpdate.Before(now) {
		e.info("Stale response, updating immediately")
		return true
	}
	if e.maxAge > 0 {
		// cache max age has expired
		if e.lastSync.Add(e.maxAge).Before(now) {
			e.info("max-age has expired, updating immediately")
			return true
		}
	}

	// update window is last quarter of NextUpdate - ThisUpdate
	// TODO: support using NextPublish instead of ThisUpdate if provided
	// in responses
	windowSize := e.nextUpdate.Sub(e.thisUpdate) / 4
	updateWindowStarts := e.nextUpdate.Add(-windowSize)
	if updateWindowStarts.After(now) {
		return false
	}

	// randomly pick time in update window
	updateTime := updateWindowStarts.Add(time.Second * time.Duration(mrand.Intn(int(windowSize.Seconds()))))
	if updateTime.Before(now) {
		e.info("Time to update")
		return true
	}
	return false
}
