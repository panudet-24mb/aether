package domain

import (
	"encoding/binary"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A site (building) may carry one 3D model the digital twin draws: a CAD export the browser converted to binary
// glTF (.glb). It arrives in chunks because every request body stays under the 256 KiB API limit.
const (
	// SiteModelMaxSize caps one model. Every upload is written to the WAL and the PITR archive, so it stays small.
	SiteModelMaxSize = 32 << 20
	// SiteModelChunkSize is the raw bytes of one chunk: its base64 JSON body (about 213 KiB) stays well under the
	// 256 KiB body limit of the API and the 288 KiB of Caddy.
	SiteModelChunkSize = 160 << 10
	// SiteModelUploadTTL is how long an unfinished upload stays usable.
	SiteModelUploadTTL = time.Hour
	// SiteModelOpenUploads caps the unfinished uploads of one workspace (each up to SiteModelMaxSize).
	SiteModelOpenUploads = 4
	// SiteModelCommitSlots caps the commits running at once across all workspaces (each assembles up to 32 MiB in
	// the database); a commit that finds no free slot is ErrUnavailable.
	SiteModelCommitSlots = 3
)

// SiteModelUploadInProgress is the conflict of starting an upload while another member's unfinished upload to the
// same site is still usable: it names that upload so the page can offer to cancel it.
type SiteModelUploadInProgress struct {
	UploadID  string
	StartedAt time.Time
}

func (e *SiteModelUploadInProgress) Error() string { return "upload_in_progress" }
func (e *SiteModelUploadInProgress) Unwrap() error { return ErrConflict }

// SiteModelFormats is what the browser converted from; the stored bytes are always GLB.
var SiteModelFormats = []string{"glb", "gltf", "obj", "fbx", "stl", "3dm", "ifc"}

// SiteModelChunks is the number of chunks an upload of size bytes is split into.
func SiteModelChunks(size int) int { return (size + SiteModelChunkSize - 1) / SiteModelChunkSize }

// ModelTransform places the model on the site: the model's own unit scaled to metres, which axis is up, a rotation
// and an offset in metres from the floor plan origin, and how it is drawn.
type ModelTransform struct {
	Scale       float64   `json:"scale"`
	Up          string    `json:"up"`
	RotationDeg []float64 `json:"rotation_deg"`
	OffsetM     []float64 `json:"offset_m"`
	Opacity     float64   `json:"opacity"`
	Visible     bool      `json:"visible"`
	Mode        string    `json:"mode"`
	ClipToFloor bool      `json:"clip_to_floor"`
}

func DefaultModelTransform() ModelTransform {
	return ModelTransform{Scale: 1, Up: "y", RotationDeg: []float64{0, 0, 0}, OffsetM: []float64{0, 0, 0}, Opacity: 1, Visible: true, Mode: "overlay", ClipToFloor: true}
}

func within(v, lo, hi float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= lo && v <= hi }

func (t ModelTransform) Validate() error {
	if !within(t.Scale, 0.0001, 10000) || !within(t.Opacity, 0.05, 1) || (t.Up != "y" && t.Up != "z") || (t.Mode != "overlay" && t.Mode != "replace") ||
		len(t.RotationDeg) != 3 || len(t.OffsetM) != 3 {
		return ErrInvalid
	}
	for i := 0; i < 3; i++ {
		if !within(t.RotationDeg[i], -360, 360) || !within(t.OffsetM[i], -100000, 100000) {
			return ErrInvalid
		}
	}
	return nil
}

// SiteModel is the metadata of a site's model; the bytes are served by GET /sites/:id/model/file.
type SiteModel struct {
	Revision     int            `json:"revision"`
	Name         string         `json:"name"`
	SourceFormat string         `json:"source_format"`
	Size         int            `json:"size"`
	SHA256       string         `json:"sha256"`
	UploadedAt   time.Time      `json:"uploaded_at"`
	Transform    ModelTransform `json:"transform"`
}

// SiteModelUpload is a started upload: the declared file and the chunks it is expected in.
type SiteModelUpload struct {
	ID           string `json:"upload_id"`
	SiteID       string `json:"-"`
	Name         string `json:"-"`
	SourceFormat string `json:"-"`
	Size         int    `json:"-"`
	SHA256       string `json:"-"`
	ChunkSize    int    `json:"chunk_size"`
	Chunks       int    `json:"chunks"`
}

// CleanModelName keeps the file name a person sees: no path, no control or bidi-override characters, trimmed.
// It returns false when nothing is left or the name is longer than 120 characters.
func CleanModelName(s string) (string, bool) {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	return s, s != "" && utf8.RuneCountInString(s) <= 120
}

// ValidGLBHeader checks the first 20 bytes of a binary glTF of size bytes: magic "glTF", version 2, the header's
// total length equal to size, and a first chunk of type JSON that fits inside the file.
func ValidGLBHeader(head []byte, size int) bool {
	if len(head) < 20 || size < 20 {
		return false
	}
	le := binary.LittleEndian
	jsonLen := int64(le.Uint32(head[12:16]))
	return string(head[0:4]) == "glTF" && le.Uint32(head[4:8]) == 2 && int64(le.Uint32(head[8:12])) == int64(size) &&
		le.Uint32(head[16:20]) == 0x4E4F534A && jsonLen > 0 && 20+jsonLen <= int64(size)
}
