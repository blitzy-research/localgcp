package gcs

import (
	"encoding/json"
	"net/http"
	"strings"
)

// maxComposeSourceObjects is the number of source objects a single compose
// request may name. Cloud Storage accepts between 1 and 32 sources, so 32 is
// valid and 33 is rejected with 400.
const maxComposeSourceObjects = 32

// composeRequest models the supported compose body. Only sourceObjects is
// required; encoding/json ignores unmodeled fields such as kind and
// deleteSourceObjects for client compatibility.
type composeRequest struct {
	SourceObjects []composeSourceObject `json:"sourceObjects"`
	Destination   *composeDestination   `json:"destination"`
}

type composeSourceObject struct {
	Name string `json:"name"`
}

// composeDestination carries the optional destination properties. Only
// ContentType is honored: the destination object name always comes from the URL
// path, and metadata is deliberately left unmodeled so the decoder skips it
// instead of allocating a map the emulator's object schema cannot store. Leaving
// it out still accepts the field, because the decoder tolerates unknown fields.
type composeDestination struct {
	ContentType string `json:"contentType"`
}

// handleComposeObject parses a prefix-stripped compose path. Source-count
// validation precedes store access, and query parameters are ignored because
// clients append alt=json&prettyPrint=false.
func (s *Service) handleComposeObject(w http.ResponseWriter, r *http.Request, rest string) {
	// Format: {bucket}/o/{destinationObject}/compose — the destination name may
	// itself contain slashes, so split only on the first /o/.
	parts := strings.SplitN(rest, "/o/", 2)
	if len(parts) != 2 || parts[0] == "" {
		writeBadRequest(w, "Invalid compose path")
		return
	}
	bucket := parts[0]

	// Strip only the trailing token, so a destination legitimately named
	// "x/compose" still resolves.
	dstName := strings.TrimSuffix(parts[1], "/compose")
	if dstName == "" {
		writeBadRequest(w, "Destination object name is required")
		return
	}

	// The decoder stays in its default, non-strict mode so unmodeled fields such
	// as kind, deleteSourceObjects and destination.metadata remain tolerated for
	// client compatibility.
	var req composeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "Invalid compose request body")
		return
	}

	if len(req.SourceObjects) == 0 {
		writeBadRequest(w, "The compose request requires at least 1 source object")
		return
	}
	if len(req.SourceObjects) > maxComposeSourceObjects {
		writeBadRequest(w, "The compose request accepts at most 32 source objects")
		return
	}

	// Preserve the requested order: no sorting and no de-duplication.
	srcNames := make([]string, 0, len(req.SourceObjects))
	for _, src := range req.SourceObjects {
		if src.Name == "" {
			writeBadRequest(w, "Each source object requires a name")
			return
		}
		srcNames = append(srcNames, src.Name)
	}

	// Level 1 of the content-type fallback. An empty value lets the store adopt
	// the first source's content type, which in turn falls back to the
	// emulator's application/octet-stream default.
	contentType := ""
	if req.Destination != nil {
		contentType = req.Destination.ContentType
	}

	obj, err := s.store.ComposeObject(bucket, dstName, srcNames, contentType)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeNotFound(w, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internalError", err.Error())
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(obj)
}
