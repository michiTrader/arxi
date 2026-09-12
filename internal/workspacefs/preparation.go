package workspacefs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
)

const (
	PreparationFile   = "workspace-provisioning.v1.ndjson"
	preparationSchema = "arxi.workspace-provisioning/v1"
)

type preparationRecord struct {
	Schema  string  `json:"schema"`
	Phase   string  `json:"phase"`
	Digest  string  `json:"request_digest"`
	Request Request `json:"request"`
}

// Prepare durably brackets every pre-accept workspace side effect. Recovery
// re-enters Provision for both started and finished records: existence is never
// adoption evidence, while a verified provisioner result is.
func Prepare(ctx context.Context, dir string, provisioner Provisioner, requests []Request) error {
	if provisioner == nil && len(requests) != 0 {
		return errors.New("workspace preparation requires a provisioner")
	}
	path := filepath.Join(dir, PreparationFile)
	records, err := readPreparation(path)
	if err != nil {
		return err
	}
	phases, err := validatePreparation(records, requests)
	if err != nil {
		return err
	}
	for _, req := range requests {
		digest, err := requestDigest(req)
		if err != nil {
			return err
		}
		if phases[digest] == "" {
			if err := appendPreparation(path, preparationRecord{Schema: preparationSchema, Phase: "prepared", Digest: digest, Request: req}); err != nil {
				return err
			}
			phases[digest] = "prepared"
		}
	}
	for _, req := range requests {
		digest, _ := requestDigest(req)
		if phases[digest] == "prepared" {
			if err := appendPreparation(path, preparationRecord{Schema: preparationSchema, Phase: "started", Digest: digest, Request: req}); err != nil {
				return err
			}
			phases[digest] = "started"
		}
		if _, err := provisioner.Provision(ctx, req); err != nil {
			return fmt.Errorf("provision workspace for %q: %w", req.Member, err)
		}
		if phases[digest] != "finished" {
			if err := appendPreparation(path, preparationRecord{Schema: preparationSchema, Phase: "finished", Digest: digest, Request: req}); err != nil {
				return err
			}
			phases[digest] = "finished"
		}
	}
	return nil
}

// AbortPreparation releases only exactly recorded, provisioner-verified
// ownership. It is for failures before acceptance, where no run outcome exists
// whose evidence must be retained.
func AbortPreparation(ctx context.Context, dir string, provisioner Provisioner, requests []Request) error {
	if provisioner == nil || len(requests) == 0 {
		return nil
	}
	records, err := readPreparation(filepath.Join(dir, PreparationFile))
	if err != nil {
		return err
	}
	phases, err := validatePreparation(records, requests)
	if err != nil {
		return err
	}
	var result error
	for _, req := range requests {
		digest, _ := requestDigest(req)
		if phases[digest] != "started" && phases[digest] != "finished" {
			continue
		}
		session, provisionErr := provisioner.Provision(ctx, req)
		if provisionErr != nil {
			result = errors.Join(result, fmt.Errorf("verify workspace for %q before abort: %w", req.Member, provisionErr))
			continue
		}
		if releaseErr := provisioner.Release(ctx, req, session); releaseErr != nil {
			result = errors.Join(result, fmt.Errorf("release workspace for %q after refused acceptance: %w", req.Member, releaseErr))
		}
	}
	return result
}

func validatePreparation(records []preparationRecord, requests []Request) (map[string]string, error) {
	wanted := map[string]Request{}
	for _, req := range requests {
		digest, err := requestDigest(req)
		if err != nil {
			return nil, err
		}
		if _, exists := wanted[digest]; exists {
			return nil, fmt.Errorf("workspace preparation contains duplicate request %s", digest)
		}
		wanted[digest] = req
	}
	phases := map[string]string{}
	for _, record := range records {
		if record.Schema != preparationSchema {
			return nil, fmt.Errorf("workspace preparation schema %q is unsupported", record.Schema)
		}
		want, ok := wanted[record.Digest]
		if !ok || !reflect.DeepEqual(want, record.Request) {
			return nil, fmt.Errorf("workspace preparation contains a request outside this submission")
		}
		prior := phases[record.Digest]
		valid := prior == "" && record.Phase == "prepared" || prior == "prepared" && record.Phase == "started" || prior == "started" && record.Phase == "finished"
		if !valid {
			return nil, fmt.Errorf("workspace request %s has invalid %s to %s transition", record.Digest, prior, record.Phase)
		}
		phases[record.Digest] = record.Phase
	}
func requestDigest(req Request) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encode workspace request: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func readPreparation(path string) ([]preparationRecord, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open workspace preparation record: %w", err)
	}
	defer file.Close()
	var out []preparationRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		dec := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		dec.DisallowUnknownFields()
		var record preparationRecord
		if err := dec.Decode(&record); err != nil {
			return nil, fmt.Errorf("decode workspace preparation record: %w", err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("decode workspace preparation record: trailing JSON value")
		}
		out = append(out, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read workspace preparation record: %w", err)
	}
	return out, nil
}

func appendPreparation(path string, record preparationRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode workspace preparation record: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open workspace preparation record: %w", err)
	}
	if _, err := file.Write(append(body, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("append workspace preparation record: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync workspace preparation record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close workspace preparation record: %w", err)
	}
	return nil
}
