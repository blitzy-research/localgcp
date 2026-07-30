package gcs

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxComposeSourceObjects is the number of source objects a single compose
// request may name. Cloud Storage accepts between 1 and 32 sources, so 32 is
// valid and 33 is rejected with 400.
const maxComposeSourceObjects = 32

// maxComposeRequestBytes caps how much of a compose body is read before it is
// decoded. maxComposeSourceObjects alone cannot bound the work a request costs,
// because that check can only run once the whole sourceObjects array has already
// been materialized; capping the bytes is what keeps an oversized array, source
// name or ignored field from driving unbounded decoder allocation and CPU on
// this unauthenticated endpoint.
//
// The cap sits far above any legitimate request: Cloud Storage object names are
// at most 1024 bytes, so 32 maximum-length sources occupy roughly 34 KiB of
// JSON, and the accepted-but-ignored fields add only a few KiB more. 1 MiB
// therefore leaves ample headroom while still bounding the decoder.
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

// writeComposeBodyError refuses a compose body through the shared 400 envelope,
// so the client sees a well-formed GCS error rather than a truncated response.
// An oversized body earns its own message; everything else — including a nil
// error, which means a second JSON value decoded where end-of-body was required
// — is reported as a malformed body.
//
// errors.As rather than a type assertion, because the decoder is free to wrap the
// error the bounded reader returns; it is also nil-safe, which is what lets the
// nil case fall through to the malformed message.
func writeComposeBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeBadRequest(w, "The compose request body exceeds the maximum size")
		return
	}
	writeBadRequest(w, "Invalid compose request body")
}

// handleComposeObject parses a prefix-stripped compose path. Source-count
// validation precedes store access, and query parameters are ignored because
// clients append alt=json&prettyPrint=false.
//
// escapedRest is the remainder in its ESCAPED form. The decoded form cannot be
// used: r.URL.Path has already turned every %2F into "/", which makes a
// destination name that contains "/copyTo/b/" or ends in "/compose"
// indistinguishable from a structural sub-operation. Keeping the escapes lets
// the structural tokens be matched first, after which each component is
// percent-decoded on its own — so an encoded slash stays part of the name it was
// written in rather than becoming a separator.
func (s *Service) handleComposeObject(w http.ResponseWriter, r *http.Request, escapedRest string) {
	// Format: {bucket}/o/{destinationObject}/compose — the destination name may
	// itself contain slashes, so split only on the first /o/.
	parts := strings.SplitN(escapedRest, "/o/", 2)
	if len(parts) != 2 || parts[0] == "" {
		writeBadRequest(w, "Invalid compose path")
		return
	}
	bucket, err := url.PathUnescape(parts[0])
	if err != nil {
		writeBadRequest(w, "Invalid compose path")
		return
	}

	// Strip only the trailing structural token, so a destination legitimately
	// named "x/compose" — which a client sends as "x%2Fcompose/compose" — still
	// resolves to "x/compose".
	escapedName := strings.TrimSuffix(parts[1], "/compose")
	if escapedName == "" {
		writeBadRequest(w, "Destination object name is required")
		return
	}
	dstName, err := url.PathUnescape(escapedName)
	if err != nil {
		writeBadRequest(w, "Invalid destination object name")
		return
	}

	// Refuse a body that declares itself over the cap before a single byte of it
	// is read. MaxBytesReader below catches the same case, but only after
	// transferring the whole allowance, and a chunked request declares no length
	// at all — so this is an early exit, not the bound itself.
	if r.ContentLength > maxComposeRequestBytes {
		writeBadRequest(w, "The compose request body exceeds the maximum size")
		return
	}

	// Bound the body before decoding it. The decoder stays in its default,
	// non-strict mode so unmodeled fields such as kind, deleteSourceObjects and
	// destination.metadata remain tolerated for client compatibility.
	r.Body = http.MaxBytesReader(w, r.Body, maxComposeRequestBytes)

	// One decoder for both reads, because the second read must resume exactly
	// where the first stopped — including anything the first already buffered.
	dec := json.NewDecoder(r.Body)

	var req composeRequest
	if err := dec.Decode(&req); err != nil {
		writeComposeBodyError(w, err)
		return
	}

	// A compose body is exactly ONE JSON document, so nothing but optional
	// whitespace may follow it. Without this read the request is only validated
	// as far as its first value: a second document would be silently ignored
	// (`{...}{"sourceObjects":[]}` would compose from the first), trailing garbage
	// would pass as well-formed, and the byte cap would only ever have applied to
	// the prefix the decoder happened to consume. io.EOF is therefore the only
	// acceptable outcome — a nil error means a second value decoded, which is
	// malformed too.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeComposeBodyError(w, err)
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
