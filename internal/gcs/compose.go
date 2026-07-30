package gcs

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxComposeSourceObjects is the number of source objects a single compose
// request may name. Cloud Storage accepts between 1 and 32 sources, so 32 is
// valid and 33 is rejected with 400.
const maxComposeSourceObjects = 32

// maxComposeRequestBytes caps how much of a compose body is read. The source
// limit above cannot bound the work a request costs, because it can only be
// applied once the whole sourceObjects array has already been materialized;
// capping the bytes is what keeps an oversized array, source name or
// accepted-but-ignored field from driving unbounded decoder allocation on this
// unauthenticated endpoint.
//
// The cap sits far above any legitimate request: Cloud Storage object names are
// at most 1024 bytes, so 32 maximum-length sources occupy roughly 34 KiB of
// JSON and the ignored fields add a few KiB more. 1 MiB therefore leaves ample
// headroom while still bounding the decoder.
const maxComposeRequestBytes = 1 << 20

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

// handleComposeObject parses a prefix-stripped compose path. The body is read
// under a byte bound and must contain exactly one JSON document; source-count
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

	// Bound the body before decoding it, so an unauthenticated request cannot
	// make the decoder read and materialize an arbitrarily large document. An
	// over-cap body surfaces as *http.MaxBytesError, which is reported through
	// the same shared 400 envelope as any other unusable body; errors.As is used
	// rather than a type assertion because the decoder is free to wrap it.
	r.Body = http.MaxBytesReader(w, r.Body, maxComposeRequestBytes)

	// One decoder for both reads below, so the second resumes exactly where the
	// first stopped — including anything the first already buffered. The decoder
	// stays in its default, non-strict mode so unmodeled fields such as kind,
	// deleteSourceObjects and destination.metadata remain tolerated for client
	// compatibility.
	dec := json.NewDecoder(r.Body)

	var tooLarge *http.MaxBytesError

	var req composeRequest
	if err := dec.Decode(&req); err != nil {
		if errors.As(err, &tooLarge) {
			writeBadRequest(w, "The compose request body exceeds the maximum size")
		} else {
			writeBadRequest(w, "Invalid compose request body")
		}
		return
	}

	// A compose body is exactly ONE JSON document, so nothing but optional
	// whitespace may follow it and io.EOF is the only acceptable outcome of the
	// next read. Without this the request would only ever be validated as far as
	// its first value: a second document would be silently ignored while the
	// first composed, trailing garbage would pass as well-formed, and the byte cap
	// would only cover the prefix the decoder happened to consume. A nil error
	// means a second value decoded, which is malformed too. Trailing whitespace is
	// not a value, so clients that encode with json.Encoder — which appends a
	// newline — keep working.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if errors.As(err, &tooLarge) {
			writeBadRequest(w, "The compose request body exceeds the maximum size")
		} else {
			writeBadRequest(w, "Invalid compose request body")
		}
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
