// Copyright 2017 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
package storage

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/kr/pretty"
	"github.com/pkg/errors"
)

func entryEq(l, r raftpb.Entry) error {
	if reflect.DeepEqual(l, r) {
		return nil
	}
	_, lData := DecodeRaftCommand(l.Data)
	_, rData := DecodeRaftCommand(r.Data)
	var lc, rc storagebase.RaftCommand
	if err := lc.Unmarshal(lData); err != nil {
		return errors.Wrap(err, "unmarshalling LHS")
	}
	if err := rc.Unmarshal(rData); err != nil {
		return errors.Wrap(err, "unmarshalling RHS")
	}
	if !reflect.DeepEqual(lc, rc) {
		return errors.New(strings.Join(pretty.Diff(lc, rc), "\n"))
	}
	return nil
}

func mkEnt(
	v raftCommandEncodingVersion, index, term uint64, as *storagebase.ReplicatedEvalResult_AddSSTable,
) raftpb.Entry {
	cmdIDKey := strings.Repeat("x", raftCommandIDLen)
	var cmd storagebase.RaftCommand
	cmd.ReplicatedEvalResult.AddSSTable = as
	b, err := cmd.Marshal()
	if err != nil {
		panic(err)
	}
	var ent raftpb.Entry
	ent.Index, ent.Term = index, term
	ent.Data = encodeRaftCommand(v, storagebase.CmdIDKey(cmdIDKey), b)
	return ent
}

func TestRaftSSTableSideloadingInline(t *testing.T) {
	defer leaktest.AfterTest(t)()

	v1, v2 := raftVersionStandard, raftVersionSideloaded
	rangeID := roachpb.RangeID(1)

	type testCase struct {
		// Entry passed into maybeInlineSideloadedRaftCommand and the entry
		// after having (perhaps) been modified.
		fat, thin raftpb.Entry
		// Populate the raft entry cache and sideload storage before running the test.
		setup func(*raftEntryCache, sideloadStorage)
		// If nonempty, the error expected from maybeInlineSideloadedRaftCommand.
		expErr string
		// If nonempty, a regex that the recorded trace span must match.
		expTrace string
	}

	sstFat := storagebase.ReplicatedEvalResult_AddSSTable{
		Data:  []byte("foo"),
		CRC32: 0, // not checked
	}
	sstThin := storagebase.ReplicatedEvalResult_AddSSTable{
		CRC32: 0, // not checked
	}

	putOnDisk := func(ec *raftEntryCache, ss sideloadStorage) {
		if err := ss.PutIfNotExists(context.Background(), 5, 6, sstFat.Data); err != nil {
			t.Fatal(err)
		}
	}

	testCases := map[string]testCase{
		// Plain old v1 Raft command without payload. Don't touch.
		"v1-no-payload": {fat: mkEnt(v1, 5, 6, &sstThin), thin: mkEnt(v1, 5, 6, &sstThin)},
		// With payload, but command is v1. Don't touch. Note that the
		// first of the two shouldn't happen in practice or we have a
		// huge problem once we try to apply this entry.
		"v1-slim-with-payload": {fat: mkEnt(v1, 5, 6, &sstThin), thin: mkEnt(v1, 5, 6, &sstThin)},
		"v1-with-payload":      {fat: mkEnt(v1, 5, 6, &sstFat), thin: mkEnt(v1, 5, 6, &sstFat)},
		// v2 with payload, but payload is AWOL. This would be fatal in practice.
		"v2-with-payload-missing-file": {
			fat: mkEnt(v2, 5, 6, &sstThin), thin: mkEnt(v2, 5, 6, &sstThin),
			expErr: "not found",
		},
		// v2 with payload that's actually there. The request we'll see in
		// practice.
		"v2-with-payload-with-file-no-cache": {
			fat: mkEnt(v2, 5, 6, &sstThin), thin: mkEnt(v2, 5, 6, &sstFat),
			setup: putOnDisk, expTrace: "inlined entry not cached",
		},
		"v2-with-payload-with-file-with-cache": {
			fat: mkEnt(v2, 5, 6, &sstThin), thin: mkEnt(v2, 5, 6, &sstFat),
			setup: func(ec *raftEntryCache, ss sideloadStorage) {
				putOnDisk(ec, ss)
				ec.addEntries(rangeID, []raftpb.Entry{mkEnt(v2, 5, 6, &sstFat)})
			}, expTrace: "using cache hit",
		},
	}

	runOne := func(k string, test testCase) {
		ctx, collect := testutils.MakeRecordCtx()

		ec := newRaftEntryCache(1024) // large enough
		ss := newInMemSideloadStorage(rangeID, roachpb.ReplicaID(1), ".")
		if test.setup != nil {
			test.setup(ec, ss)
		}

		fatCopy := *(protoutil.Clone(&test.fat).(*raftpb.Entry))
		newEnt, err := maybeInlineSideloadedRaftCommand(ctx, rangeID, fatCopy, ss, ec)
		if err != nil {
			if test.expErr == "" || !testutils.IsError(err, test.expErr) {
				t.Fatalf("%s: %s", k, err)
			}
		} else if test.expErr != "" {
			t.Fatalf("%s: success, but expected error: %s", k, test.expErr)
		} else if err := entryEq(fatCopy, test.fat); err != nil {
			t.Fatalf("%s: mutated the original entry: %s", k, pretty.Diff(fatCopy, test.fat))
		}

		if newEnt == nil {
			newEnt = &fatCopy
		}
		if err := entryEq(*newEnt, test.thin); err != nil {
			t.Fatalf("%s: %s", k, err)
		}

		if dump := collect(); test.expTrace != "" {
			if ok, err := regexp.MatchString(test.expTrace, dump); err != nil {
				t.Fatalf("%s: %s", k, err)
			} else if !ok {
				t.Fatalf("%s: expected trace matching:\n%s\n\nbut got\n%s", k, test.expTrace, dump)
			}
		}
	}

	keys := make([]string, 0, len(testCases))
	for k := range testCases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		runOne(k, testCases[k])
	}
}

func TestRaftSSTableSideloadingInflight(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx, collect := testutils.MakeRecordCtx()
	sideloaded := newInMemSideloadStorage(roachpb.RangeID(5), roachpb.ReplicaID(7), ".")

	// We'll set things up so that while sideloading this entry, there
	// unmarshaled one is already in memory (so the payload here won't even be
	// looked at).
	preEnts := []raftpb.Entry{mkEnt(raftVersionSideloaded, 7, 1, &storagebase.ReplicatedEvalResult_AddSSTable{
		Data:  []byte("not the payload you're looking for"),
		CRC32: 0, // not checked
	})}

	origBytes := []byte("compare me")

	// Pretend there's an inflight command that actually has an SSTable in it.
	var pendingCmd storagebase.RaftCommand
	pendingCmd.ReplicatedEvalResult.AddSSTable = &storagebase.ReplicatedEvalResult_AddSSTable{
		Data: origBytes, CRC32: 0, // not checked
	}
	maybeCmd := func(cmdID storagebase.CmdIDKey) (storagebase.RaftCommand, bool) {
		return pendingCmd, true
	}

	// The entry should be recognized as "to be sideloaded", then maybeCmd is
	// invoked and supplies the RaftCommand, whose SSTable is then persisted.
	postEnts, err := maybeSideloadEntriesImpl(ctx, preEnts, sideloaded, maybeCmd)
	if err != nil {
		t.Fatal(err)
	}

	if len(postEnts) != 1 {
		t.Fatalf("expected exactly one entry: %+v", postEnts)
	}

	if b, err := sideloaded.Get(ctx, preEnts[0].Index, preEnts[0].Term); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(b, origBytes) {
		t.Fatalf("expected payload %s, got %s", origBytes, b)
	}

	re := regexp.MustCompile(`(?ms)copying entries slice of length 1.*command already in memory.*writing payload`)
	if trace := collect(); !re.MatchString(trace) {
		t.Fatalf("trace did not match %s:\n%s", re, trace)
	}
}

func TestRaftSSTableSideloadingSideload(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	noCmd := func(storagebase.CmdIDKey) (cmd storagebase.RaftCommand, ok bool) {
		return
	}

	addSST := storagebase.ReplicatedEvalResult_AddSSTable{
		Data: []byte("foo"), CRC32: 0, // not checked
	}

	addSSTStripped := addSST
	addSSTStripped.Data = nil

	entV1Reg := mkEnt(raftVersionStandard, 10, 99, nil)
	entV1SST := mkEnt(raftVersionStandard, 11, 99, &addSST)
	entV2Reg := mkEnt(raftVersionSideloaded, 12, 99, nil)
	entV2SST := mkEnt(raftVersionSideloaded, 13, 99, &addSST)
	entV2SSTStripped := mkEnt(raftVersionSideloaded, 13, 99, &addSSTStripped)

	type tc struct {
		name              string
		preEnts, postEnts []raftpb.Entry
		ss                []string
	}

	// Intentionally ignore the fact that real calls would always have an
	// unbroken run of `entry.Index`.
	testCases := []tc{
		{name: "empty", preEnts: nil, postEnts: nil, ss: nil},
		{name: "v1", preEnts: []raftpb.Entry{entV1Reg, entV1SST}, postEnts: []raftpb.Entry{entV1Reg, entV1SST}},
		{
			name:     "v2",
			preEnts:  []raftpb.Entry{entV2SST, entV2Reg},
			postEnts: []raftpb.Entry{entV2SSTStripped, entV2Reg},
			ss:       []string{"i13t99"},
		},
		{
			name:     "mixed",
			preEnts:  []raftpb.Entry{entV1Reg, entV1SST, entV2Reg, entV2SST},
			postEnts: []raftpb.Entry{entV1Reg, entV1SST, entV2Reg, entV2SSTStripped},
			ss:       []string{"i13t99"},
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			sideloaded := newInMemSideloadStorage(roachpb.RangeID(3), roachpb.ReplicaID(17), ".")
			postEnts, err := maybeSideloadEntriesImpl(ctx, test.preEnts, sideloaded, noCmd)
			if err != nil {
				t.Fatal(err)
			}
			if len(addSST.Data) == 0 {
				t.Fatal("invocation mutated original AddSSTable struct in memory")
			}
			if !reflect.DeepEqual(postEnts, test.postEnts) {
				t.Fatalf("result differs from expected: %s", pretty.Diff(postEnts, test.postEnts))
			}
			var actKeys []string
			for k := range sideloaded.(*inMemSideloadStorage).m {
				actKeys = append(actKeys, fmt.Sprintf("i%dt%d", k.index, k.term))
			}
			sort.Strings(actKeys)
			if !reflect.DeepEqual(actKeys, test.ss) {
				t.Fatalf("expected %v, got %v", test.ss, actKeys)
			}
		})
	}
}

func setMockAddSSTable() (undo func()) {
	prev := commands[roachpb.AddSSTable]

	// TODO(tschottdorf): this already does nontrivial work. Worth open-sourcing the relevant
	// subparts of the real evalAddSSTable to make this test less likely to rot.
	evalAddSSTable := func(
		ctx context.Context, batch engine.ReadWriter, cArgs CommandArgs, _ roachpb.Response,
	) (EvalResult, error) {
		log.Event(ctx, "evaluated testing-only AddSSTable mock")
		args := cArgs.Args.(*roachpb.AddSSTableRequest)

		return EvalResult{
			Replicated: storagebase.ReplicatedEvalResult{
				AddSSTable: &storagebase.ReplicatedEvalResult_AddSSTable{
					Data:  args.Data,
					CRC32: util.CRC32(args.Data),
				},
			},
		}, nil
	}

	SetAddSSTableCmd(Command{
		DeclareKeys: DefaultDeclareKeys,
		Eval:        evalAddSSTable,
	})
	return func() {
		SetAddSSTableCmd(prev)
	}
}

func makeSSTable(key, value string, ts hlc.Timestamp) ([]byte, engine.MVCCKeyValue) {
	sst, err := engine.MakeRocksDBSstFileWriter()
	if err != nil {
		panic(err)
	}
	defer sst.Close()

	v := roachpb.MakeValueFromBytes([]byte(value))
	v.InitChecksum([]byte(key))

	kv := engine.MVCCKeyValue{
		Key: engine.MVCCKey{
			Key:       []byte(key),
			Timestamp: ts,
		},
		Value: v.RawBytes,
	}

	if err := sst.Add(kv); err != nil {
		panic(errors.Wrap(err, "while finishing SSTable"))
	}
	b, err := sst.Finish()
	if err != nil {
		panic(errors.Wrap(err, "while finishing SSTable"))
	}
	return b, kv
}

func proposeAddSSTable(
	ctx context.Context, key, val string, ts hlc.Timestamp, tc *testContext,
) engine.MVCCKeyValue {
	t := tc.TB
	kv := func() engine.MVCCKeyValue {
		var ba roachpb.BatchRequest
		ba.RangeID = tc.repl.RangeID

		var addReq roachpb.AddSSTableRequest
		var kv engine.MVCCKeyValue
		addReq.Data, kv = makeSSTable(key, val, ts)
		addReq.Key = roachpb.Key(key)
		addReq.EndKey = addReq.Key.Next()
		ba.Add(&addReq)

		_, pErr := tc.store.Send(ctx, ba)
		if pErr != nil {
			t.Fatal(pErr)
		}
		return kv
	}()

	return kv
}

// TestRaftSSTableSideloadingProposal runs a straightforward application of an `AddSSTable` command.
func TestRaftSSTableSideloadingProposal(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer setMockAddSSTable()()

	tc := testContext{}
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	tc.Start(t, stopper)

	ctx, collect := testutils.MakeRecordCtx()
	defer collect()

	const (
		key = "foo"
		val = "bar"
	)

	ts := hlc.Timestamp{Logical: 1}

	proposeAddSSTable(ctx, key, val, ts, &tc)

	{
		var ba roachpb.BatchRequest
		get := getArgs(roachpb.Key(key))
		ba.Add(&get)
		ba.Header.RangeID = tc.repl.RangeID

		br, pErr := tc.store.Send(ctx, ba)
		if pErr != nil {
			t.Fatal(pErr)
		}
		v := br.Responses[0].GetInner().(*roachpb.GetResponse).Value
		if v == nil {
			t.Fatal("expected to read a value")
		}
		if valBytes, err := v.GetBytes(); err != nil {
			t.Fatal(err)
		} else if !bytes.Equal(valBytes, []byte(val)) {
			t.Fatalf("expected to read '%s', but found '%s'", val, valBytes)
		}
	}

	tc.repl.raftMu.Lock()
	defer tc.repl.raftMu.Unlock()
	if imss := tc.repl.raftMu.sideloaded.(*inMemSideloadStorage); len(imss.m) < 1 {
		t.Fatal("sideloaded storage is empty")
	}

	if err := testutils.MatchInOrder(collect(), "sideloadable proposal detected", "ingested SSTable"); err != nil {
		t.Fatal(err)
	}
}

type mockSender struct {
	logEntries [][]byte
	done       bool
}

func (mr *mockSender) Send(req *SnapshotRequest) error {
	if req.LogEntries != nil {
		if mr.logEntries != nil {
			return errors.New("already have log entries")
		}
		mr.logEntries = req.LogEntries
	}
	return nil
}

func (mr *mockSender) Recv() (*SnapshotResponse, error) {
	if mr.done {
		return nil, io.EOF
	}
	status := SnapshotResponse_ACCEPTED
	if len(mr.logEntries) > 0 {
		status = SnapshotResponse_APPLIED
		mr.done = true
	}
	return &SnapshotResponse{Status: status}, nil
}

// This test verifies that when a snapshot is sent, sideloaded proposals are
// inlined.
func TestRaftSSTableSideloadingSnapshot(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer setMockAddSSTable()()

	ctx := context.Background()
	tc := testContext{}
	stopper := stop.NewStopper()
	defer stopper.Stop(ctx)
	tc.Start(t, stopper)

	var ba roachpb.BatchRequest
	ba.RangeID = tc.repl.RangeID

	// Disable log truncation as we want to be sure that we get to create
	// snapshots that have our sideloaded proposal in them.
	tc.store.SetRaftLogQueueActive(false)

	// Put a sideloaded proposal on the Range.
	key, val := "don't", "care"
	origSSTData, _ := makeSSTable(key, val, hlc.Timestamp{}.Add(0, 1))
	{

		var addReq roachpb.AddSSTableRequest
		addReq.Data = origSSTData
		addReq.Key = roachpb.Key(key)
		addReq.EndKey = addReq.Key.Next()
		ba.Add(&addReq)

		_, pErr := tc.store.Send(ctx, ba)
		if pErr != nil {
			t.Fatal(pErr)
		}
	}

	// Run a happy case snapshot. Check that it properly inlines the payload in
	// the contained log entries.
	inlinedEntry := func() raftpb.Entry {
		os, err := tc.repl.GetSnapshot(ctx, "testing-will-succeed")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Close()

		mockSender := &mockSender{}
		if err := sendSnapshot(
			ctx,
			mockSender,
			&fakeStorePool{},
			SnapshotRequest_Header{State: os.State, Priority: SnapshotRequest_RECOVERY},
			os,
			tc.repl.store.Engine().NewBatch,
			func() {},
		); err != nil {
			t.Fatal(err)
		}

		var ent raftpb.Entry
		var cmd storagebase.RaftCommand
		var finalEnt raftpb.Entry
		for _, entryBytes := range mockSender.logEntries {
			if err := ent.Unmarshal(entryBytes); err != nil {
				t.Fatal(err)
			}
			if sniffSideloadedRaftCommand(ent.Data) {
				_, cmdBytes := DecodeRaftCommand(ent.Data)
				if err := cmd.Unmarshal(cmdBytes); err != nil {
					t.Fatal(err)
				}
				if as := cmd.ReplicatedEvalResult.AddSSTable; as == nil {
					t.Fatalf("no AddSSTable found in sideloaded command %+v", cmd)
				} else if len(as.Data) == 0 {
					t.Fatalf("empty payload in sideloaded command: %+v", cmd)
				}
				finalEnt = ent
			}
		}
		if finalEnt.Index == 0 {
			t.Fatal("no sideloaded command found")
		}
		return finalEnt
	}()

	sideloadedIndex := inlinedEntry.Index

	// This happens to be a good point in time to check the `entries()` method
	// which has special handling to accommodate `term()`: when an empty
	// sideload storage is passed in, `entries()` should not inline, and in turn
	// also not populate the entries cache (since its contents must always be
	// fully inlined).
	func() {
		tc.repl.raftMu.Lock()
		defer tc.repl.raftMu.Unlock()
		tc.repl.mu.Lock()
		defer tc.repl.mu.Unlock()
		for _, withSS := range []bool{false, true} {
			tc.store.raftEntryCache.clearTo(tc.repl.RangeID, sideloadedIndex+1)

			var ss sideloadStorage
			if withSS {
				ss = tc.repl.raftMu.sideloaded
			}
			entries, err := entries(
				ctx, tc.store.Engine(), tc.repl.RangeID, tc.store.raftEntryCache, ss, sideloadedIndex, sideloadedIndex+1, 1<<20,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("no or too many entries returned from cache: %+v", entries)
			}
			ents, _, _ := tc.store.raftEntryCache.getEntries(nil, tc.repl.RangeID, sideloadedIndex, sideloadedIndex+1, 1<<20)
			if withSS {
				// We passed the sideload storage, so we expect to get our
				// inlined index back from the cache.
				if len(ents) != 1 {
					t.Fatalf("no or too many entries returned from cache: %+v", ents)
				}
				if err := entryEq(inlinedEntry, ents[0]); err != nil {
					t.Fatalf("withSS=%t: %s", withSS, err)
				}
			} else {
				// Without sideload storage, expect the cache to remain
				// unpopulated and the entry returned from entries() to not have
				// been inlined.
				if len(ents) != 0 {
					t.Fatalf("expected no cached entries, but got %+v", ents)
				}
				if expErr, err := `ReplicatedEvalResult.AddSSTable.Data: \[\]uint8\[\d+\] != \[\]uint8\[0\]`,
					entryEq(inlinedEntry, entries[0]); !testutils.IsError(
					err,
					expErr,
				) {
					t.Fatalf("expected specific mismatch on `Data` field, but got %v\nwanted: %s", err, expErr)
				}
			}
		}
	}()

	// Now run a snapshot that will fail since it doesn't find one of its on-disk
	// payloads. This can happen if the Raft log queue runs between the time the
	// (engine) snapshot is taken and the log entries are actually read from the
	// (engine) snapshot. We didn't run this before because we wanted the file
	// to stay in sideloaded storage for the previous test.
	func() {
		failingOS, err := tc.repl.GetSnapshot(ctx, "testing-will-fail")
		if err != nil {
			t.Fatal(err)
		}
		defer failingOS.Close()

		// Remove the actual file.
		tc.repl.raftMu.Lock()
		if err := tc.repl.raftMu.sideloaded.Clear(ctx); err != nil {
			tc.repl.raftMu.Unlock()
			t.Fatal(err)
		}
		tc.repl.raftMu.Unlock()
		// Additionally we need to clear out the entry from the cache because
		// that would still save the day.
		tc.store.raftEntryCache.clearTo(tc.repl.RangeID, sideloadedIndex+1)

		mockSender := &mockSender{}
		if err := sendSnapshot(
			ctx,
			mockSender,
			&fakeStorePool{},
			SnapshotRequest_Header{State: failingOS.State, Priority: SnapshotRequest_RECOVERY},
			failingOS,
			tc.repl.store.Engine().NewBatch,
			func() {},
		); errors.Cause(err) != errMustRetrySnapshotDueToTruncation {
			t.Fatal(err)
		}
	}()
}

func TestRaftSSTableSideloadingTruncation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer setMockAddSSTable()()

	tc := testContext{}
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	tc.Start(t, stopper)
	ctx := context.Background()

	const count = 10

	var indexes []uint64
	addLastIndex := func() {
		lastIndex, err := tc.repl.GetLastIndex()
		if err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, lastIndex)
	}
	for i := 0; i < count; i++ {
		addLastIndex()
		key := fmt.Sprintf("key-%d", i)
		val := fmt.Sprintf("val-%d", i)
		proposeAddSSTable(ctx, key, val, tc.Clock().Now(), &tc)
	}
	// Append an extra entry which, if we truncate it, should definitely also
	// remove any leftover files (ok, unless the last one is reproposed but
	// that's *very* unlikely to happen for the last one)
	addLastIndex()

	fmtSideloaded := func() []string {
		var r []string
		tc.repl.raftMu.Lock()
		defer tc.repl.raftMu.Unlock()
		for k := range tc.repl.raftMu.sideloaded.(*inMemSideloadStorage).m {
			r = append(r, fmt.Sprintf("%v", k))
		}
		sort.Strings(r)
		return r
	}

	// Check that when we truncate, the number of on-disk files changes in ways
	// we expect. Intentionally not too strict due to the possibility of
	// reproposals, etc; it could be made stricter, but this should give enough
	// confidence already that we're calling `PurgeTo` correctly, and for the
	// remainder unit testing on each impl's PurgeTo is more useful.
	for i := range indexes {
		newFirstIndex := indexes[i] + 1
		truncateArgs := truncateLogArgs(newFirstIndex, rangeID)
		log.Eventf(ctx, "truncating to index < %d", newFirstIndex)
		if _, pErr := client.SendWrappedWith(ctx, tc.Sender(), roachpb.Header{RangeID: 1}, &truncateArgs); pErr != nil {
			t.Fatal(pErr)
		}
		sideloadStrings := fmtSideloaded()
		if minFiles := count - i; len(sideloadStrings) < minFiles {
			t.Fatalf("after truncation at %d (i=%d), expected at least %d files left, but have:\n%v",
				indexes[i], i, minFiles, sideloadStrings)
		}
	}

	if sideloadStrings := fmtSideloaded(); len(sideloadStrings) != 0 {
		t.Fatalf("expected all files to be cleaned up, but found %v", sideloadStrings)
	}

}