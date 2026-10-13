package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"
)

type SContext struct {
	w   http.ResponseWriter
	req *http.Request
}

type fieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c SContext) Resp(resp map[string]any) { c.JSON(http.StatusOK, resp) }

func (c SContext) JSON(status int, body any) {
	c.w.Header().Set("Content-Type", "application/json")
	c.writeJSON(status, body)
}

func (c SContext) writeJSON(status int, body any) {
	c.w.WriteHeader(status)
	if err := json.NewEncoder(c.w).Encode(body); err != nil {
		slog.ErrorContext(c.req.Context(), "write response",
			"error", err, "request_id", c.w.Header().Get("X-Request-Id"),
			"method", c.req.Method, "path", c.req.URL.Path, "status", status,
		)
	}
}

func (c SContext) Problem(status int, code, detail string) {
	c.problem(status, map[string]any{"code": code, "detail": detail}, nil)
}

func (c SContext) problem(status int, body map[string]any, err error) {
	level := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	cause := body["detail"]
	if err != nil {
		cause = err
	}
	slog.Log(c.req.Context(), level, "request failed",
		"error", cause, "request_id", c.w.Header().Get("X-Request-Id"),
		"method", c.req.Method, "path", c.req.URL.Path, "status", status, "code", body["code"],
	)
	body["type"], body["title"], body["status"] = "about:blank", http.StatusText(status), status
	c.w.Header().Set("Content-Type", "application/problem+json")
	c.writeJSON(status, body)
}

func (c SContext) invalid(path, code, message string) bool {
	c.problem(http.StatusUnprocessableEntity, map[string]any{
		"code": "validation_failed", "detail": "One or more request fields are invalid.",
		"errors": []fieldError{{Path: path, Code: code, Message: message}},
	}, errors.New(message))
	return false
}

func (c SContext) Bind(target any, allowed ...string) bool {
	mediaType, _, err := mime.ParseMediaType(c.req.Header.Get("Content-Type"))
	encoding := c.req.Header.Get("Content-Encoding")
	unsupportedEncoding := encoding != "" && encoding != "identity"
	if err != nil || mediaType != "application/json" || unsupportedEncoding {
		c.Problem(http.StatusUnsupportedMediaType, "unsupported_media_type", "Use uncompressed application/json.")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.w, c.req.Body, 16<<10))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		c.Problem(http.StatusRequestEntityTooLarge, "body_too_large", "The request body exceeds 16 KiB.")
		return false
	}
	if err != nil || !utf8.Valid(body) || !json.Valid(body) {
		c.problem(http.StatusBadRequest, map[string]any{
			"code": "invalid_json", "detail": "The request body must contain one valid JSON object.",
		}, err)
		return false
	}
	if err := uniqueKeys(json.NewDecoder(bytes.NewReader(body))); err != nil {
		c.Problem(http.StatusBadRequest, "invalid_json", "Duplicate JSON keys are not allowed.")
		return false
	}
	object := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return c.invalid("", "invalid_object", "Provide a JSON object.")
	}
	for key := range object {
		if !slices.Contains(allowed, key) {
			return c.invalid("/"+pointerEscape(key), "unknown_field", "Unknown request field.")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return c.invalid("", "invalid_field", "Request fields have invalid names or types.")
	}
	return true
}

func pointerEscape(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func uniqueKeys(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid key")
			}
			seen[name] = true
		}
		if err := uniqueKeys(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}
