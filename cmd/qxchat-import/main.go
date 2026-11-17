package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/go-redis/redis/v8"
)

type importRow struct {
	Status    string                     `json:"status"`
	SourceID  string                     `json:"source_id,omitempty"`
	RequestID string                     `json:"request_id,omitempty"`
	Profile   *model.QXChatSourceProfile `json:"profile,omitempty"`
	Mapping   *service.QXChatImportState `json:"mapping,omitempty"`
	Code      string                     `json:"code,omitempty"`
}

func readImportRows(path string) ([]importRow, error) {
	info, err := os.Lstat(path)
	if err != nil || !filepath.IsAbs(path) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("input must be an absolute private regular file (0600)")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open private input")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private input changed while opening")
	}
	var rows []importRow
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 65536)
	for scanner.Scan() {
		var row importRow
		if len(rows) >= 100 || common.Unmarshal(scanner.Bytes(), &row) != nil {
			return nil, errors.New("input must contain at most 100 valid JSONL records")
		}
		sourceID := row.SourceID
		if row.Profile != nil {
			sourceID = row.Profile.SourceID
		} else if row.Mapping != nil {
			sourceID = row.Mapping.SourceID
		}
		if sourceID == "" || seen[sourceID] {
			return nil, errors.New("each input record must have a unique source_id")
		}
		seen[sourceID] = true
		rows = append(rows, row)
	}
	if scanner.Err() != nil || len(rows) == 0 {
		return nil, errors.New("input is empty or exceeds the JSONL record limit")
	}
	return rows, nil
}

func processImportRow(ctx context.Context, phase string, apply bool, input importRow) (importRow, error) {
	result := importRow{Status: "rejected", SourceID: input.SourceID, RequestID: input.RequestID}
	if input.Profile != nil {
		result.SourceID, result.RequestID = input.Profile.SourceID, input.Profile.RequestID
	} else if input.Mapping != nil {
		result.SourceID, result.RequestID = input.Mapping.SourceID, input.Mapping.RequestID
	}
	if input.Status == "rejected" {
		result.Code = "source_rejected"
		return result, nil
	}
	var state *service.QXChatImportState
	var err error
	switch {
	case phase == "inspect" && input.Mapping != nil && (input.Status == "prepared" || input.Status == "active"):
		state, err = service.InspectQXChatImport(*input.Mapping)
	case phase == "activate" && input.Mapping != nil && (input.Status == "prepared" || input.Status == "active"):
		state, err = service.ActivateQXChatImport(ctx, *input.Mapping, apply)
	case input.Status == "mapped" && input.Mapping != nil && phase != "activate":
		if input.Mapping.Authority != "newapi" {
			result.Code = "source_authority_mismatch"
			return result, nil
		}
		state, err = service.InspectQXChatImport(*input.Mapping)
	case phase != "activate" && input.Profile != nil && (input.Status == "prepared" || input.Status == "eligible"):
		if apply && input.Status != "prepared" {
			result.Code = "source_reservation_required"
			return result, nil
		}
		var mapping *model.QXChatIdentity
		var user *model.User
		mapping, user, err = model.PrepareQXChatImport(*input.Profile, apply)
		if err == nil {
			state = &service.QXChatImportState{QXChatCommitRequest: service.QXChatCommitRequest{
				SourceID: mapping.SourceID, RequestID: mapping.RequestID, NewAPIUserID: mapping.UserID,
				CredentialVersion: mapping.CredentialVersion, AuthorityVersion: mapping.AuthorityVersion}, State: mapping.State, Imported: mapping.Imported,
				TargetUsername: user.Username, TargetEmail: user.Email}
			if mapping.State == model.QXChatIdentityPending && mapping.UserID > 0 {
				state.PasswordDigest = model.QXChatImportPasswordDigest(user.Password)
			}
		}
	default:
		result.Code = "invalid_phase_record"
		return result, nil
	}
	if err != nil {
		switch {
		case errors.Is(err, model.ErrQXChatIdentitySnapshotConflict):
			result.Code = "pending_snapshot_mismatch"
		case errors.Is(err, model.ErrQXChatIdentityConflict):
			result.Code = "target_identity_collision"
		case errors.Is(err, model.ErrQXChatIdentityInactive):
			result.Code = "target_or_source_ineligible"
		case errors.Is(err, model.ErrInvalidCredentials):
			result.Code = "source_commit_rejected"
		case errors.Is(err, service.ErrQXChatUnavailable):
			result.Code = "source_unavailable"
			return result, errors.New("source unavailable; resume from recorded state")
		default:
			result.Code = "target_operation_failed"
			return result, errors.New("target operation failed; inspect recorded state before resuming")
		}
		return result, nil
	}
	result.Mapping = state
	result.Status = "prepared"
	if state.NewAPIUserID == 0 {
		result.Status = "eligible"
	} else if state.State == model.QXChatIdentityActive {
		result.Status = "active"
	}
	return result, nil
}

func runImport() error {
	phase := flag.String("phase", "inspect", "inspect, prepare, or activate")
	inputPath := flag.String("input", "", "absolute private JSONL input")
	outputPath := flag.String("output", "", "new absolute private JSONL result")
	expectedDB := flag.String("expected-db", "", "expected database name")
	expectedServer := flag.String("expected-server-id", "", "MySQL server_uuid, PostgreSQL address:port, or absolute SQLite file")
	apply := flag.Bool("apply", false, "apply prepare or activate; otherwise only inspect")
	dryRun := flag.Bool("dry-run", false, "explicit read-only run")
	flag.Parse()
	if (*phase != "inspect" && *phase != "prepare" && *phase != "activate") || (*apply && (*dryRun || *phase == "inspect")) || flag.NArg() != 0 {
		return errors.New("invalid phase or apply/dry-run combination")
	}
	if !filepath.IsAbs(*outputPath) || *outputPath == *inputPath {
		return errors.New("output must be a new absolute path distinct from input")
	}
	rows, err := readImportRows(*inputPath)
	if err != nil {
		return err
	}
	output, err := os.OpenFile(*outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create new private output; existing files are never overwritten")
	}
	defer output.Close()
	if err := model.OpenQXChatImportDB(*expectedDB, *expectedServer); err != nil {
		return err
	}
	connection, err := model.DB.DB()
	if err != nil {
		return errors.New("target connection unavailable")
	}
	defer connection.Close()
	common.RedisEnabled = false
	if *apply && *phase == "activate" {
		if !service.QXChatIdentityEnabled() {
			return errors.New("source identity service configuration is required for activation")
		}
		if os.Getenv("REDIS_CONN_STRING") == "" {
			return errors.New("explicit REDIS_CONN_STRING is required to publish activated accounts")
		}
		options, err := redis.ParseURL(os.Getenv("REDIS_CONN_STRING"))
		if err != nil {
			return errors.New("invalid Redis configuration")
		}
		options.PoolSize = 2
		common.RDB = redis.NewClient(options)
		defer common.RDB.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = common.RDB.Ping(ctx).Err()
		cancel()
		if err != nil {
			return errors.New("Redis is unavailable; no account activation attempted")
		}
		common.RedisEnabled = true
	}
	counts := map[string]int{}
	for _, row := range rows {
		result, operationErr := processImportRow(context.Background(), *phase, *apply, row)
		encoded, err := common.Marshal(result)
		if err != nil {
			return errors.New("cannot encode private result")
		}
		if _, err := output.Write(append(encoded, '\n')); err != nil {
			return errors.New("cannot persist private result; inspect database before resuming")
		}
		if err := output.Sync(); err != nil {
			return errors.New("cannot sync private result; inspect database before resuming")
		}
		counts[result.Status]++
		if operationErr != nil {
			summary, _ := common.Marshal(map[string]any{"phase": *phase, "apply": *apply, "counts": counts, "complete": false})
			fmt.Println(string(summary))
			return operationErr
		}
	}
	summary, _ := common.Marshal(map[string]any{"phase": *phase, "apply": *apply, "counts": counts, "complete": true})
	fmt.Println(string(summary))
	return nil
}

func main() {
	if err := runImport(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
