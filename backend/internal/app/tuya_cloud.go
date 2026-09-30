package app

import (
	"aether/backend/internal/adapters/tuyacloud"
	"aether/backend/internal/domain"
	"aether/backend/internal/security"
	"context"
	"errors"
	"strings"
)

// GatewayModels is the catalog's gateway list: the Tuya Cloud model only when this deployment enabled it.
func (s *Service) GatewayModels() []domain.GatewayModel {
	out := make([]domain.GatewayModel, 0, len(domain.GatewayModels))
	for _, m := range domain.GatewayModels {
		if m.ID == domain.TuyaCloudGatewayModel && !s.TuyaCloudEnabled {
			continue
		}
		out = append(out, m)
	}
	return out
}

// DeviceProfiles is the catalog's profile list: the Tuya Cloud and Tuya BLE profiles only when this deployment
// enabled them.
func (s *Service) DeviceProfiles() []domain.DeviceProfile {
	out := make([]domain.DeviceProfile, 0, len(domain.DeviceProfiles))
	for _, p := range domain.DeviceProfiles {
		if (p.ID == domain.TuyaCloudProfile && !s.TuyaCloudEnabled) || (p.ID == domain.TuyaBLEProfile && !s.EdgeBLEEnabled) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// cloudClient builds the OpenAPI client that proves a link's credentials (tests substitute the fake cloud).
func (s *Service) cloudClient(region, accessID, accessSecret string) (*tuyacloud.Client, error) {
	if s.CloudClient != nil {
		return s.CloudClient(region, accessID, accessSecret)
	}
	return tuyacloud.New(region, accessID, accessSecret)
}

// LinkTuyaCloud links a Tuya Cloud gateway to the member's cloud project, or rotates its credentials. The contract
// the HTTP route (G4) relies on, in this order:
//  1. Tuya Cloud is enabled here, the member may manage devices, and the API holds the worker's public key
//     (otherwise 503 tuya_cloud_unconfigured);
//  2. region, channel and the credentials' shape are valid;
//  3. the credentials are proven with Tuya (GET /v1.0/token) BEFORE anything is written, so nobody can claim a
//     project's Access ID (its digest is globally unique) without holding its secret;
//  4. they are sealed to the worker's public key for this tenant and gateway, and saved with a hint and a digest of
//     the Access ID. The API can never read them back.
func (s *Service) LinkTuyaCloud(ctx context.Context, p domain.Principal, gateway, region, channel, accessID, accessSecret string) error {
	if !s.TuyaCloudEnabled {
		return domain.Because(domain.ErrInvalid, "tuya_cloud_disabled")
	}
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if s.TuyaCloudPublicKey == nil {
		return domain.Because(domain.ErrUnavailable, "tuya_cloud_unconfigured")
	}
	if !security.ValidID(gateway) {
		return domain.ErrInvalid
	}
	if _, ok := tuyacloud.Regions[region]; !ok {
		return domain.Because(domain.ErrInvalid, "tuya_region")
	}
	if channel == "" {
		channel = tuyacloud.ChannelProd
	}
	if channel != tuyacloud.ChannelProd && channel != tuyacloud.ChannelTest {
		return domain.Because(domain.ErrInvalid, "tuya_channel")
	}
	accessID, accessSecret = strings.TrimSpace(accessID), strings.TrimSpace(accessSecret)
	if !tuyaCredential.MatchString(accessID) || !tuyaCredential.MatchString(accessSecret) {
		return domain.Because(domain.ErrInvalid, "tuya_credentials")
	}
	model, e := s.Repo.GatewayModel(ctx, p, gateway)
	if e != nil {
		return e
	}
	if model != domain.TuyaCloudGatewayModel {
		return domain.Because(domain.ErrInvalid, "not_a_cloud_gateway")
	}
	client, e := s.cloudClient(region, accessID, accessSecret)
	if e != nil {
		return domain.Because(domain.ErrInvalid, "tuya_credentials")
	}
	if e := client.Authenticate(ctx); e != nil {
		reason, _ := importError(e)
		switch {
		case errors.Is(e, tuyacloud.ErrUnavailable) || errors.Is(e, context.DeadlineExceeded):
			return domain.Because(domain.ErrUnavailable, reason)
		case errors.Is(e, tuyacloud.ErrRateLimited):
			return domain.Because(domain.ErrRateLimited, reason)
		}
		return domain.Because(domain.ErrInvalid, reason)
	}
	sealed, e := security.SealTuyaCloud(s.TuyaCloudPublicKey, p.TenantID, gateway, accessID, accessSecret)
	if e != nil {
		return e
	}
	return s.Repo.SaveTuyaCloudLink(ctx, p, domain.TuyaCloudLinkRequest{GatewayID: gateway, Region: region, Channel: channel,
		AccessIDHint: accessID[:4], AccessIDDigest: security.AccessIDDigest(accessID), CredentialsSealed: sealed})
}

// UnlinkTuyaCloud forgets a gateway's project credentials. It works with Tuya Cloud disabled too, so a deployment
// that turns the mode off can still clean up.
func (s *Service) UnlinkTuyaCloud(ctx context.Context, p domain.Principal, gateway string) error {
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if !security.ValidID(gateway) {
		return domain.ErrInvalid
	}
	return s.Repo.UnlinkTuyaCloud(ctx, p, gateway)
}

// RequestTuyaCloudSync asks the worker to list the project's devices again.
func (s *Service) RequestTuyaCloudSync(ctx context.Context, p domain.Principal, gateway string) error {
	if !s.TuyaCloudEnabled {
		return domain.Because(domain.ErrInvalid, "tuya_cloud_disabled")
	}
	if !p.CanManageDevices() {
		return domain.ErrForbidden
	}
	if !security.ValidID(gateway) {
		return domain.ErrInvalid
	}
	return s.Repo.RequestCloudSync(ctx, p, gateway)
}

// TuyaCloudLinkStatus is what the gateway page shows: never a credential.
func (s *Service) TuyaCloudLinkStatus(ctx context.Context, p domain.Principal, gateway string) (domain.TuyaCloudLink, error) {
	if !security.ValidID(gateway) {
		return domain.TuyaCloudLink{}, domain.ErrInvalid
	}
	out, e := s.Repo.TuyaCloudLinkStatus(ctx, p, gateway)
	if e != nil {
		return out, e
	}
	out.Enabled, out.EventsBudget, out.APICallsBudget = s.TuyaCloudEnabled, s.TuyaCloudEventBudget, s.TuyaCloudAPIBudget
	return out, nil
}
