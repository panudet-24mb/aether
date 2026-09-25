package ports

import (
	"aether/backend/internal/adapters/minew"
	"aether/backend/internal/adapters/zigbee2mqtt"
	"aether/backend/internal/automation"
	"aether/backend/internal/domain"
	"aether/backend/internal/signals"
	"aether/backend/internal/studio"
	"context"
	"encoding/json"
	"time"
)

type Repository interface {
	MemberAccess(context.Context, domain.Principal, string) (map[string]string, error)
	SetMemberAccess(context.Context, domain.Principal, string, map[string]string) error
	DiscoverDevices(context.Context, domain.Principal, string, time.Time) ([]domain.DiscoveredDevice, error)
	ListProjects(context.Context, domain.Principal, bool) ([]domain.Project, error)
	CreateProject(context.Context, domain.Principal, domain.Project) error
	UpdateProject(context.Context, domain.Principal, domain.Project) error
	ArchiveProject(context.Context, domain.Principal, string) error
	SetGatewayProject(context.Context, domain.Principal, string, *string) error
	SetDeviceRoaming(context.Context, domain.Principal, string, bool) error
	ListSites(context.Context, domain.Principal) ([]domain.Site, error)
	GetSite(context.Context, domain.Principal, string) (domain.Site, error)
	CreateSite(context.Context, domain.Principal, domain.Site) (domain.Site, error)
	UpdateSite(context.Context, domain.Principal, domain.Site) error
	ArchiveSite(context.Context, domain.Principal, string) error
	CreateFloor(context.Context, domain.Principal, domain.Floor) (domain.Floor, error)
	SaveFloor(context.Context, domain.Principal, domain.Floor) (int, map[string]int, error)
	DeleteFloor(context.Context, domain.Principal, string) error
	SetFloorImage(context.Context, domain.Principal, string, string, []byte) error
	FloorImage(context.Context, domain.Principal, string) (string, []byte, error)
	Presence(context.Context, domain.Principal, string) (domain.Presence, error)
	// Asset registry: every gateway and active device with its record, maintenance plans and history.
	ListAssets(context.Context, domain.Principal) ([]domain.AssetRow, error)
	GetAsset(context.Context, domain.Principal, string, string) (domain.AssetDetail, error)
	UpsertAssetRecord(context.Context, domain.Principal, domain.AssetRecord) error
	CreatePlan(context.Context, domain.Principal, domain.MaintenancePlan) error
	UpdatePlan(context.Context, domain.Principal, domain.MaintenancePlan) error
	DeletePlan(context.Context, domain.Principal, string) error
	AddLog(context.Context, domain.Principal, domain.MaintenanceLog) error
	// Alerts, events, rules and notification channels (see internal/alerts).
	ActiveTenants(context.Context) ([]string, error)
	ScanOffline(context.Context, string, time.Time) (int, error)
	ScanVacancy(context.Context, string, time.Time) (int, error)
	ClaimNotifications(context.Context, string, int, time.Time) ([]domain.NotificationJob, error)
	FinishNotification(context.Context, string, string, string, string) error
	PruneAlertData(context.Context, string) error
	SetChannelEnabled(context.Context, domain.Principal, string, bool) error
	ListEvents(context.Context, domain.Principal, string, int) ([]domain.DeviceEvent, error)
	ListAlerts(context.Context, domain.Principal, string, int) ([]domain.Alert, error)
	AlertCounts(context.Context, domain.Principal) (map[string]int, error)
	TransitionAlert(context.Context, domain.Principal, string, string, string) error
	ListRules(context.Context, domain.Principal) ([]domain.AlertRule, error)
	// BuiltinRuleIDs: the rules Aether seeded for this workspace, for labelling only.
	BuiltinRuleIDs(context.Context, domain.Principal) ([]string, error)
	SaveRule(context.Context, domain.Principal, domain.AlertRule, bool) error
	DeleteRule(context.Context, domain.Principal, string) error
	ListChannels(context.Context, domain.Principal) ([]domain.NotificationChannel, error)
	CreateChannel(context.Context, domain.Principal, domain.NotificationChannel, string) error
	DeleteChannel(context.Context, domain.Principal, string) error
	ChannelWithSecret(context.Context, domain.Principal, string) (domain.NotificationChannel, string, error)
	ListNotifications(context.Context, domain.Principal, int) ([]domain.Notification, error)

	EnrollMQTT(context.Context, domain.Principal, string, string, bool) error
	MQTTStates(context.Context, domain.Principal) ([]domain.MQTTState, error)
	MQTTGatewayTenant(context.Context, string) (string, error)
	GatewayModel(context.Context, domain.Principal, string) (string, error)
	BLEHistory(context.Context, domain.Principal, string, string, time.Time) ([]studio.Observation, error)
	StudioList(context.Context, domain.Principal, string) ([]studio.Item, error)
	StudioGet(context.Context, domain.Principal, string) (studio.Item, error)
	StudioCreate(context.Context, domain.Principal, studio.Item) error
	StudioSave(context.Context, domain.Principal, studio.Item) error
	StudioDelete(context.Context, domain.Principal, string) error

	ListTemplates(context.Context, domain.Principal) ([]domain.DeviceTemplate, error)
	CreateTemplate(context.Context, domain.Principal, domain.DeviceTemplate) error
	SetStreamTemplate(context.Context, domain.Principal, string, string, string, string) error
	StreamHistory(context.Context, domain.Principal, string, time.Time, int) ([]minew.Sensor, error)

	CreateAccount(context.Context, domain.User, string, string, bool) error
	UserByEmail(context.Context, string) (domain.User, error)
	StartSession(context.Context, domain.Session, string) (domain.Session, error)
	RotateRefresh(context.Context, string, string, string, time.Time) (domain.Session, error)
	Authorize(context.Context, domain.Principal) (domain.Principal, error)
	RevokeSession(context.Context, domain.Principal) error
	CreateGateway(context.Context, domain.Principal, domain.Gateway, string) error
	ListGateways(context.Context, domain.Principal) ([]domain.Gateway, error)
	RevokeGateway(context.Context, domain.Principal, string) error
	GatewayTenant(context.Context, string, string) (string, error)
	CreateDevice(context.Context, domain.Principal, domain.Device) error
	ListDevices(context.Context, domain.Principal) ([]domain.Device, error)
	ListRemovedDevices(context.Context, domain.Principal) ([]domain.Device, error)
	UpdateDevice(context.Context, domain.Principal, string, *string, *string) (domain.Device, error)
	RemoveDevice(context.Context, domain.Principal, string) error
	RestoreDevice(context.Context, domain.Principal, string) error
	DeviceState(context.Context, domain.Principal, string) (domain.State, error)
	CapturePacket(context.Context, string, string, json.RawMessage) (string, error)
	CaptureZ2M(context.Context, string, string, zigbee2mqtt.Message, []byte) (string, error)
	QueueCommand(context.Context, domain.Principal, domain.CommandRequest) (domain.Command, bool, error)
	ListCommands(context.Context, domain.Principal, string, int) ([]domain.Command, error)
	GetCommand(context.Context, domain.Principal, string) (domain.Command, error)
	DeviceControls(context.Context, domain.Principal, string) (domain.DeviceControls, error)
	ListPackets(context.Context, domain.Principal, string) ([]domain.Packet, error)
	StoreTelemetry(context.Context, string, string, string, time.Time, map[string]float64) (domain.TelemetryEvent, error)

	// Automation Studio: block flows evaluated on every uplink (see internal/automation).
	ListAutomations(context.Context, domain.Principal) ([]automation.Automation, error)
	GetAutomation(context.Context, domain.Principal, string) (automation.Automation, error)
	CreateAutomation(context.Context, domain.Principal, automation.Automation) error
	SaveAutomation(context.Context, domain.Principal, automation.Automation) error
	SetAutomationEnabled(context.Context, domain.Principal, string, bool) error
	DeleteAutomation(context.Context, domain.Principal, string) error
	ListAutomationRuns(context.Context, domain.Principal, string, int) ([]automation.Run, error)
	AutomationChannels(context.Context, domain.Principal) (map[string]bool, error)
	// AutomationOptions loads what automation.Check needs for one definition (channels, command targets and
	// their validator, the AUTOMATION_COMMANDS switch, whether this member may control devices).
	AutomationOptions(ctx context.Context, p domain.Principal, project *string, definition json.RawMessage, enabled bool) (automation.Options, error)
	// CommandableDevices lists the devices an automation of `project` (nil = workspace) may command.
	CommandableDevices(context.Context, domain.Principal, *string) ([]domain.CommandableDevice, error)
	TestAutomation(context.Context, domain.Principal, string, automation.TestInput) (automation.Result, error)

	// Members of the workspace and the project access the database enforces for them
	// (see migration 00019 and docs/platform/team-access.md).
	ListMembers(context.Context, domain.Principal) ([]domain.Member, error)
	// AddMember refuses (ErrConflict) an email whose identity already belongs to any workspace.
	AddMember(context.Context, domain.Principal, domain.NewMember) (domain.Member, error)
	UpdateMember(context.Context, domain.Principal, string, string, []string) error
	RemoveMember(context.Context, domain.Principal, string) error
	ResetMemberPassword(context.Context, domain.Principal, string, string) error
	// ChangeOwnPassword verifies the current hash through the callback, so Argon2id stays in the service.
	ChangeOwnPassword(context.Context, domain.Principal, func(string) bool, string) error
	MemberSelf(context.Context, domain.Principal) (domain.MemberSelf, error)

	// Learned device signals ("สอนสัญญาณให้ระบบ"): the operator records the device at rest and while
	// being triggered, and the difference becomes a matcher evaluated at ingest. See internal/signals
	// for the analysis and docs/platform/minew-kit.md for why guessing the encoding is not an option.
	CreateSignalSession(context.Context, domain.Principal, signals.NewSession) (signals.Session, error)
	SignalSession(context.Context, domain.Principal, string) (signals.Session, error)
	AdvanceSignalSession(context.Context, domain.Principal, string) (signals.Session, error)
	CancelSignalSession(context.Context, domain.Principal, string) error
	ConfirmSignalSession(context.Context, domain.Principal, string, int, string) (signals.Signal, error)
	ListSignals(context.Context, domain.Principal) ([]signals.Signal, error)
	DeleteSignal(context.Context, domain.Principal, string) error
	// TestSignal replays recent raw history through a stored matcher, so false positives are visible
	// before the signal is trusted.
	TestSignal(context.Context, domain.Principal, string) (signals.TestResult, error)
}
