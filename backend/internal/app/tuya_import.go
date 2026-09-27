package app

import (
	"aether/backend/internal/adapters/edge"
	"aether/backend/internal/adapters/tuya"
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// TuyaCloud is the part of the Tuya OpenAPI client an import uses. Tests replace the factory with a fake cloud;
// production always builds a tuyacloud.Client for one of the fixed region hosts.
type TuyaCloud interface {
	Authenticate(context.Context) error
	Devices(context.Context) ([]tuyacloud.Device, error)
	Model(context.Context, string) (json.RawMessage, string, error)
}

// DefaultTuyaCloud builds the real client.
func DefaultTuyaCloud(region, accessID, accessSecret string) (TuyaCloud, error) {
	return tuyacloud.New(region, accessID, accessSecret)
}

// Limits of the import job runner.
const (
	TuyaImportTimeout = 90 * time.Second
	TuyaImportTTL     = 10 * time.Minute
	maxJobsPerTenant  = 10
	EdgeInstallTTL    = 30 * time.Minute
)

// TuyaImportJob is one import as the page polls it. It never holds the Tuya credentials or a local key: those
// live only inside the goroutine that runs the import.
type TuyaImportJob struct {
	ID             string               `json:"id"`
	GatewayID      string               `json:"gateway_id"`
	Region         string               `json:"region"`
	Status         string               `json:"status"` // running | done | failed
	Stage          string               `json:"stage"`  // token | devices | models | saving | done
	Error          string               `json:"error,omitempty"`
	TuyaCode       int                  `json:"tuya_code,omitempty"`
	Hint           string               `json:"hint,omitempty"`
	SuggestRegions []string             `json:"suggest_regions,omitempty"`
	Found          int                  `json:"found"`
	Imported       int                  `json:"imported"`
	WithKey        int                  `json:"with_key"`
	LocalCapable   int                  `json:"local_capable"`
	Skipped        int                  `json:"skipped"`
	Devices        []TuyaImportedDevice `json:"devices"`
	StartedAt      time.Time            `json:"started_at"`
	FinishedAt     *time.Time           `json:"finished_at,omitempty"`
	ExpiresAt      time.Time            `json:"expires_at"`
	tenant         string
}

// TuyaImportedDevice is one device of an import result: its fingerprint, never its key.
type TuyaImportedDevice struct {
	TuyaID         string `json:"tuya_id"`
	Name           string `json:"name"`
	TuyaCategory   string `json:"tuya_category"`
	ProductID      string `json:"product_id"`
	Sub            bool   `json:"sub"`
	LocalCapable   bool   `json:"local_capable"`
	HasKey         bool   `json:"has_key"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	Spec           string `json:"spec"` // model | specifications | missing
}

// tuyaJobs keeps import jobs in memory for TuyaImportTTL. An API restart loses them: the user starts again.
type tuyaJobs struct {
	mu   sync.Mutex
	jobs map[string]*TuyaImportJob
}

func (j *tuyaJobs) purge(now time.Time) {
	for id, job := range j.jobs {
		if now.After(job.ExpiresAt) {
			delete(j.jobs, id)
		}
	}
}

func (j *tuyaJobs) start(tenant, gateway, region string, now time.Time) (*TuyaImportJob, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.purge(now)
	count := 0
	for _, job := range j.jobs {
		if job.tenant != tenant {
			continue
		}
		count++
		if job.GatewayID == gateway && job.Status == "running" {
			return nil, domain.Because(domain.ErrConflict, "import_running")
		}
	}
	if count >= maxJobsPerTenant {
		return nil, domain.Because(domain.ErrRateLimited, "too_many_imports")
	}
	job := &TuyaImportJob{ID: uuid.NewString(), GatewayID: gateway, Region: region, Status: "running", Stage: "token", Devices: []TuyaImportedDevice{},
		StartedAt: now, ExpiresAt: now.Add(TuyaImportTTL), tenant: tenant}
	j.jobs[job.ID] = job
	return job, nil
}

// update runs fn on the job under the lock.
func (j *tuyaJobs) update(id string, fn func(*TuyaImportJob)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job, ok := j.jobs[id]; ok {
		fn(job)
	}
}

func (j *tuyaJobs) get(tenant, gateway, id string, now time.Time) (TuyaImportJob, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.purge(now)
	job, ok := j.jobs[id]
	if !ok || job.tenant != tenant || job.GatewayID != gateway {
		return TuyaImportJob{}, false
	}
	out := *job
	out.Devices = append([]TuyaImportedDevice(nil), job.Devices...)
	return out, true
}

var tuyaCredential = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)

// StartTuyaImport begins a one-time key import for an Aether Edge gateway and returns at once; the page polls the
// job. The credentials are passed to the job's goroutine and to nothing else: not the job record, not the
// database, not a log.
func (s *Service) StartTuyaImport(ctx context.Context, p domain.Principal, gateway, region, accessID, accessSecret string) (TuyaImportJob, error) {
	if !p.CanManageDevices() {
		return TuyaImportJob{}, domain.ErrForbidden
	}
	if !security.ValidID(gateway) {
		return TuyaImportJob{}, domain.ErrInvalid
	}
	if _, ok := tuyacloud.Regions[region]; !ok {
		return TuyaImportJob{}, domain.Because(domain.ErrInvalid, "tuya_region")
	}
	if !tuyaCredential.MatchString(accessID) || !tuyaCredential.MatchString(accessSecret) {
		return TuyaImportJob{}, domain.Because(domain.ErrInvalid, "tuya_credentials")
	}
	model, e := s.Repo.GatewayModel(ctx, p, gateway)
	if e != nil {
		return TuyaImportJob{}, e
	}
	if model != domain.EdgeGatewayModel {
		return TuyaImportJob{}, domain.Because(domain.ErrInvalid, "not_an_edge_gateway")
	}
	factory := s.TuyaCloud
	if factory == nil {
		factory = DefaultTuyaCloud
	}
	client, e := factory(region, accessID, accessSecret)
	if e != nil {
		return TuyaImportJob{}, domain.Because(domain.ErrInvalid, "tuya_credentials")
	}
	job, e := s.tuyaJobs.start(p.TenantID, gateway, region, time.Now().UTC())
	if e != nil {
		return TuyaImportJob{}, e
	}
	snapshot, _ := s.tuyaJobs.get(p.TenantID, gateway, job.ID, time.Now().UTC())
	go s.runTuyaImport(job.ID, p, gateway, region, client)
	return snapshot, nil
}

// TuyaImportStatus returns a job of this workspace and gateway.
func (s *Service) TuyaImportStatus(p domain.Principal, gateway, id string) (TuyaImportJob, error) {
	if !p.CanManageDevices() {
		return TuyaImportJob{}, domain.ErrForbidden
	}
	job, ok := s.tuyaJobs.get(p.TenantID, gateway, id, time.Now().UTC())
	if !ok {
		return TuyaImportJob{}, domain.ErrNotFound
	}
	return job, nil
}

// importError names a failed import for the page.
func importError(e error) (string, int) {
	var api *tuyacloud.APIError
	code := 0
	if errors.As(e, &api) {
		code = api.Code
	}
	switch {
	case errors.Is(e, tuyacloud.ErrAuth):
		return "tuya_auth_failed", code
	case errors.Is(e, tuyacloud.ErrNotSubscribed):
		return "tuya_not_subscribed", code
	case errors.Is(e, tuyacloud.ErrPermission):
		return "tuya_permission", code
	case errors.Is(e, tuyacloud.ErrRateLimited):
		return "tuya_rate_limited", code
	case errors.Is(e, context.DeadlineExceeded):
		return "tuya_timeout", code
	case errors.Is(e, tuyacloud.ErrUnavailable):
		return "tuya_unreachable", code
	}
	return "tuya_error", code
}

func (s *Service) runTuyaImport(id string, p domain.Principal, gateway, region string, client TuyaCloud) {
	ctx, cancel := context.WithTimeout(context.Background(), TuyaImportTimeout)
	defer cancel()
	fail := func(reason string, code int) {
		now := time.Now().UTC()
		s.tuyaJobs.update(id, func(j *TuyaImportJob) { j.Status, j.Error, j.TuyaCode, j.FinishedAt = "failed", reason, code, &now })
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("tuya import crashed", "gateway", gateway)
			fail("internal_error", 0)
		}
	}()
	stage := func(name string) { s.tuyaJobs.update(id, func(j *TuyaImportJob) { j.Stage = name }) }
	if e := client.Authenticate(ctx); e != nil {
		fail(importError(e))
		return
	}
	stage("devices")
	devices, e := client.Devices(ctx)
	if e != nil {
		fail(importError(e))
		return
	}
	if len(devices) == 0 {
		now := time.Now().UTC()
		suggest := []string{}
		for _, r := range tuyacloud.RegionOrder {
			if r != region {
				suggest = append(suggest, r)
			}
		}
		s.tuyaJobs.update(id, func(j *TuyaImportJob) {
			j.Status, j.Stage, j.Hint, j.SuggestRegions, j.FinishedAt = "done", "done", "no_devices_try_other_region", suggest, &now
		})
		return
	}
	stage("models")
	type spec struct {
		dps      []tuya.DP
		category string
		kind     string
	}
	specs := map[string]spec{} // per product: one model call per product, not per device
	imports := []tuya.Import{}
	result := []TuyaImportedDevice{}
	skipped := 0
	for _, d := range devices {
		tuyaID := strings.ToLower(strings.TrimSpace(d.ID))
		if !edge.ValidDevice(tuyaID) {
			skipped++
			continue
		}
		key := d.ProductID
		if key == "" {
			key = "device:" + tuyaID
		}
		sp, ok := specs[key]
		if !ok {
			sp = spec{kind: "missing"}
			if raw, kind, e := client.Model(ctx, d.ID); e == nil {
				var dps []tuya.DP
				var category string
				var pe error
				if kind == "model" {
					dps, pe = tuya.ParseModel(raw)
				} else {
					dps, category, pe = tuya.ParseSpecifications(raw)
				}
				if pe == nil && tuya.Validate(dps) == nil {
					sp = spec{dps: dps, category: category, kind: kind}
				}
			} else if ctx.Err() != nil {
				fail(importError(ctx.Err()))
				return
			} else if errors.Is(e, tuyacloud.ErrRateLimited) || errors.Is(e, tuyacloud.ErrAuth) {
				fail(importError(e))
				return
			}
			specs[key] = sp
		}
		category := d.Category
		if category == "" {
			category = sp.category
		}
		imp := tuya.Import{TuyaID: tuyaID, Name: d.Name, Category: category, ProductID: d.ProductID, Sub: d.Sub, Spec: sp.dps}
		out := TuyaImportedDevice{TuyaID: tuyaID, Name: edge.Clip(d.Name, 128), TuyaCategory: edge.Clip(category, 32), ProductID: edge.Clip(d.ProductID, 64), Sub: d.Sub, Spec: sp.kind}
		if tuya.ValidLocalKey(d.LocalKey) {
			sealed, e := security.Seal(s.TuyaKeys, d.LocalKey)
			if e != nil {
				fail("internal_error", 0)
				return
			}
			imp.LocalKeySealed, imp.KeyFingerprint = sealed, tuya.Fingerprint(d.LocalKey)
			out.HasKey, out.KeyFingerprint = true, imp.KeyFingerprint
		}
		out.LocalCapable = tuya.Translate(category, sp.dps).LocalCapable && !d.Sub
		imports = append(imports, imp)
		result = append(result, out)
	}
	stage("saving")
	saved, e := s.Repo.SaveTuyaDevices(ctx, p, gateway, imports)
	if e != nil {
		slog.Warn("tuya import could not be saved", "gateway", gateway, "error_type", fmt.Sprintf("%T", e))
		fail("save_failed", 0)
		return
	}
	now := time.Now().UTC()
	s.tuyaJobs.update(id, func(j *TuyaImportJob) {
		j.Status, j.Stage, j.FinishedAt = "done", "done", &now
		j.Found, j.Imported, j.Skipped, j.Devices = len(devices), saved, skipped, result
		for _, d := range result {
			if d.HasKey {
				j.WithKey++
			}
			if d.LocalCapable {
				j.LocalCapable++
			}
		}
	})
}

// TuyaDevices lists a gateway's imported devices (no keys).
func (s *Service) TuyaDevices(ctx context.Context, p domain.Principal, gateway string) ([]domain.TuyaDevice, error) {
	if !p.CanManageDevices() {
		return nil, domain.ErrForbidden
	}
	if !security.ValidID(gateway) {
		return nil, domain.ErrInvalid
	}
	return s.Repo.TuyaDevices(ctx, p, gateway)
}

// EdgeStatus is the gateway page's view of an Aether Edge. Any member who can see the gateway may read it: it holds
// no secret, only the agent's state and counts.
func (s *Service) EdgeStatus(ctx context.Context, p domain.Principal, gateway string) (domain.EdgeStatus, error) {
	if !security.ValidID(gateway) {
		return domain.EdgeStatus{}, domain.ErrInvalid
	}
	return s.Repo.EdgeStatus(ctx, p, gateway)
}

// ForgetTuyaKey drops one imported device's local key.
func (s *Service) ForgetTuyaKey(ctx context.Context, p domain.Principal, gateway, tuyaID string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if !security.ValidID(gateway) || !edge.ValidDevice(strings.ToLower(tuyaID)) {
		return domain.ErrInvalid
	}
	return s.Repo.ForgetTuyaKey(ctx, p, gateway, tuyaID)
}

// installCodeEncoding writes a code as 26 lower-case base32 letters and digits: easy to paste, 128 bits.
var installCodeEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

var installCodePattern = regexp.MustCompile(`^[a-z2-7]{26}$`)

// CreateEdgeInstallCode issues a single-use install code for an Aether Edge gateway. Only its hash is stored; the
// code itself is returned once.
// zigbee optionally pairs a Zigbee2MQTT gateway whose credentials the installer receives too.
func (s *Service) CreateEdgeInstallCode(ctx context.Context, p domain.Principal, gateway, zigbee string) (string, time.Time, error) {
	if !p.CanManageDevices() {
		return "", time.Time{}, domain.ErrForbidden
	}
	if !security.ValidID(gateway) || (zigbee != "" && (!security.ValidID(zigbee) || zigbee == gateway)) {
		return "", time.Time{}, domain.ErrInvalid
	}
	raw := make([]byte, 16)
	if _, e := rand.Read(raw); e != nil {
		return "", time.Time{}, e
	}
	code := installCodeEncoding.EncodeToString(raw)
	expires := time.Now().UTC().Add(EdgeInstallTTL)
	if e := s.Repo.CreateEdgeInstallCode(ctx, p, gateway, zigbee, security.Digest(code), expires); e != nil {
		return "", time.Time{}, e
	}
	return code, expires, nil
}

// BootstrapEdge redeems an install code for the gateway's fresh credentials. A malformed, unknown, used or
// expired code is the same ErrUnauthorized.
func (s *Service) BootstrapEdge(ctx context.Context, code string) (domain.EdgeCredentials, error) {
	if !installCodePattern.MatchString(code) {
		return domain.EdgeCredentials{}, domain.ErrUnauthorized
	}
	password := security.RandomToken()
	hash, e := security.MQTTHash(password)
	if e != nil {
		return domain.EdgeCredentials{}, e
	}
	// The paired Zigbee2MQTT gateway's password is prepared up front (hashing is the slow part); the repository
	// applies it only when the code paired one.
	zigbeePassword := security.RandomToken()
	zigbeeHash, e := security.MQTTHash(zigbeePassword)
	if e != nil {
		return domain.EdgeCredentials{}, e
	}
	token := security.RandomToken()
	tenant, gateway, zigbee, e := s.Repo.BootstrapEdge(ctx, security.Digest(code), hash, zigbeeHash, security.Digest(token))
	if e != nil {
		return domain.EdgeCredentials{}, e
	}
	out := domain.EdgeCredentials{TenantID: tenant, GatewayID: gateway, MQTTPassword: password, HTTPToken: token}
	if zigbee != "" {
		out.ZigbeeGatewayID, out.ZigbeePassword = zigbee, zigbeePassword
	}
	return out, nil
}

// EdgeConfig is the agent's configuration: its registered, keyed devices with their local keys opened. This is
// the only place a local key leaves its seal, and it goes only to the gateway's own agent.
func (s *Service) EdgeConfig(ctx context.Context, tenant, gateway string) (int64, []domain.EdgeDevice, error) {
	revision, sealed, e := s.Repo.EdgeConfig(ctx, tenant, gateway)
	if e != nil {
		return 0, nil, e
	}
	out := make([]domain.EdgeDevice, 0, len(sealed))
	for _, d := range sealed {
		key, e := security.OpenAny(d.KeySealed, s.TuyaKeys, s.LegacyTuyaKeys)
		if e != nil || !tuya.ValidLocalKey(key) {
			slog.Warn("edge config: a local key could not be opened", "gateway", gateway, "device", d.ID)
			continue
		}
		var spec []tuya.DP
		_ = json.Unmarshal(d.Spec, &spec)
		out = append(out, domain.EdgeDevice{ID: d.ID, Key: key, Version: d.Version, IP: d.IP, Device22: d.Device22, RefreshDPs: tuya.RefreshDPs(spec)})
	}
	return revision, out, nil
}
