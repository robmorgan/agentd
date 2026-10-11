package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// Pairing runs between CLIs and daemons of different builds, so its bytes
// are pinned.
func TestPairingGoldenFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePairMessage(&buf, &PairMessage{Proof: &PairProof{MAC: []byte{1, 2, 3}}}); err != nil {
		t.Fatal(err)
	}
	payload := `{"mac":"AQID"}`
	want := append([]byte{
		0x50, 0x44, 0x47, 0x41, // magic
		0, 0, // DaemonManagementVersion
		0x33, 0x75, // 30_003 PairProof
		0, 0, 0, 0, // flags, reserved
		byte(len(payload)), 0, 0, 0,
	}, payload...)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("golden mismatch\n got % x\nwant % x", buf.Bytes(), want)
	}

	buf.Reset()
	if err := WriteManagementRequest(&buf, &ManagementRequest{Pair: Empty}); err != nil {
		t.Fatal(err)
	}
	want = []byte{0x50, 0x44, 0x47, 0x41, 0, 0, 0x23, 0x4e, 0, 0, 0, 0, 0, 0, 0, 0} // 20_003, no payload
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("pair request\n got % x\nwant % x", buf.Bytes(), want)
	}
	_, mgmt, err := ReadIncoming(bytes.NewReader(want))
	if err != nil || mgmt == nil || mgmt.Pair == nil {
		t.Fatalf("decoded %+v, %v", mgmt, err)
	}
}

func TestPairMessagesRoundTrip(t *testing.T) {
	for _, m := range []*PairMessage{
		{Hello: &PairHello{Name: "laptop", Message: []byte("a")}},
		{Challenge: &PairChallenge{Message: []byte("b"), Host: "devbox"}},
		{Proof: &PairProof{MAC: []byte("c")}},
		{Accepted: &PairAccepted{MAC: []byte("d")}},
		{Error: &ErrorResponse{Message: "wrong pairing code"}},
	} {
		var buf bytes.Buffer
		if err := WritePairMessage(&buf, m); err != nil {
			t.Fatal(err)
		}
		got, err := ReadPairMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		var again bytes.Buffer
		WritePairMessage(&again, got)
		var orig bytes.Buffer
		WritePairMessage(&orig, m)
		if !bytes.Equal(again.Bytes(), orig.Bytes()) {
			t.Fatalf("round trip changed %+v into %+v", m, got)
		}
	}
	for _, r := range []*ManagementResponse{
		{Pairing: &ManagementPairing{Code: "123-456-789", Remote: "100.64.0.5:7433", Fingerprint: "SHA256:x"}},
		{Paired: &ManagementPaired{Fingerprint: "SHA256:y", Name: "laptop", Remote: "100.64.0.9:5000"}},
	} {
		var buf bytes.Buffer
		if err := WriteManagementResponse(&buf, r); err != nil {
			t.Fatal(err)
		}
		got, err := ReadManagementResponse(&buf)
		if err != nil || (r.Pairing != nil && got.Pairing.Code != r.Pairing.Code) || (r.Paired != nil && got.Paired.Name != r.Paired.Name) {
			t.Fatalf("decoded %+v, %v", got, err)
		}
	}
}

// A pairing peer is not authenticated, so a frame announcing a large
// payload is refused before anything is allocated for it.
func TestPairMessageRefusesLargeFrames(t *testing.T) {
	header := make([]byte, frameHeaderLen)
	putFrameHeader(header, DaemonManagementVersion, kPairHello, 0, MaxPairFramePayload+1)
	if _, err := ReadPairMessage(bytes.NewReader(header)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
	// A main-protocol frame is not a pairing message.
	binary.LittleEndian.PutUint16(header[4:6], ProtocolVersion)
	binary.LittleEndian.PutUint32(header[12:16], 0)
	if _, err := ReadPairMessage(bytes.NewReader(header)); err == nil {
		t.Fatal("read a main-protocol frame as a pairing message")
	}
	if err := WritePairMessage(new(bytes.Buffer), &PairMessage{Hello: &PairHello{Name: strings.Repeat("x", MaxPairFramePayload)}}); err == nil {
		t.Fatal("wrote an oversized pairing message")
	}
}
