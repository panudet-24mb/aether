package domain

import "time"

// The asset registry is the owner's bookkeeping layer over the operational tables: every gateway and every
// active device gets one row, plus maintenance plans (PM, calibration, battery, inspection) and an
// append-only maintenance history (MA, repairs, notes). Dates travel as "YYYY-MM-DD"; "" means "not set",
// which keeps them identical in SQL (an empty string becomes NULL), in JSON and in an <input type="date">.
var (
	AssetKinds    = []string{"gateway", "device"}
	AssetStatuses = []string{"in_service", "spare", "repair", "retired"}
	PlanKinds     = []string{"pm", "calibration", "battery", "inspection", "other"}
	LogKinds      = []string{"pm", "ma", "repair", "battery", "calibration", "inspection", "note"}
)

// AssetDueSoonDays is the window in which an upcoming maintenance date is already worth showing.
const AssetDueSoonDays = 30

// PlanCompletedBy reports whether a log of this kind counts as "the round was done": a free note never does.
func PlanCompletedBy(logKind string) bool { return logKind != "note" && logKind != "" }

// AssetRecord is the editable bookkeeping row; it exists only once the owner has saved something.
type AssetRecord struct {
	AssetKind        string     `json:"asset_kind"`
	AssetID          string     `json:"asset_id"`
	SerialNo         string     `json:"serial_no"`
	AssetTag         string     `json:"asset_tag"`
	LocationNote     string     `json:"location_note"`
	Vendor           string     `json:"vendor"`
	PurchasedAt      string     `json:"purchased_at"`
	WarrantyUntil    string     `json:"warranty_until"`
	BatteryChangedAt string     `json:"battery_changed_at"`
	Status           string     `json:"status"`
	Notes            string     `json:"notes"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
}

// AssetRow is one line of the registry: operational identity merged with the record and the plan summary.
type AssetRow struct {
	AssetKind        string     `json:"asset_kind"`
	AssetID          string     `json:"asset_id"`
	Name             string     `json:"name"`
	Model            string     `json:"model"` // gateway model id, or device profile id
	ExternalID       string     `json:"external_id"`
	GatewayID        string     `json:"gateway_id"`
	GatewayName      string     `json:"gateway_name"`
	ProjectName      string     `json:"project_name"`
	LastSeen         *time.Time `json:"last_seen"`
	Battery          *float64   `json:"battery"`
	Status           string     `json:"status"`
	SerialNo         string     `json:"serial_no"`
	AssetTag         string     `json:"asset_tag"`
	LocationNote     string     `json:"location_note"`
	WarrantyUntil    string     `json:"warranty_until"`
	BatteryChangedAt string     `json:"battery_changed_at"`
	Notes            string     `json:"notes"`
	NextDue          string     `json:"next_due"`
	OverdueCount     int        `json:"overdue_count"`
	CreatedAt        time.Time  `json:"created_at"`
}

type MaintenancePlan struct {
	ID           string    `json:"id"`
	AssetKind    string    `json:"asset_kind"`
	AssetID      string    `json:"asset_id"`
	Title        string    `json:"title"`
	Kind         string    `json:"kind"`
	IntervalDays int       `json:"interval_days"`
	NextDue      string    `json:"next_due"`
	LastDone     string    `json:"last_done"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
}

type MaintenanceLog struct {
	ID          string    `json:"id"`
	AssetKind   string    `json:"asset_kind"`
	AssetID     string    `json:"asset_id"`
	PlanID      *string   `json:"plan_id"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	Detail      string    `json:"detail"`
	PerformedAt time.Time `json:"performed_at"`
	PerformedBy string    `json:"performed_by"`
	Cost        *float64  `json:"cost"`
	CreatedAt   time.Time `json:"created_at"`
}

// AssetDetail is what the drawer needs in one round trip.
type AssetDetail struct {
	Asset  AssetRow          `json:"asset"`
	Record AssetRecord       `json:"record"`
	Plans  []MaintenancePlan `json:"plans"`
	Logs   []MaintenanceLog  `json:"logs"`
}
