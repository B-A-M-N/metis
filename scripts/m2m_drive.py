#!/usr/bin/env python3
"""m2m_drive.py: Metis "Drive" adapter over the real M2M Python client.

This is the METIS_M2M_CLIENT entry point the Metis bridge invokes.
It performs NO cryptography, NO transport, and NO policy of its own:
every verified byte comes from the real client (src/m2m.py), which
owns identity, verification, replay, and acknowledgment. This file
only translates that client's verified results into the small JSON
protocol the Metis bridge documents:

  receive        -> [{message_id, sender_fp, type, expires_at, payload_b64}]
  ack <id> <state> -> {message_id, state} (after confirming the
                      sender-visible signed ack exists via ack-status)

Configuration (paths only, never secrets) comes from the environment:
  METIS_M2M_LIB    directory containing the real m2m.py
  METIS_M2M_HOME   endpoint home (identity, contacts, inbox, seen)
  METIS_M2M_RELAY  file-relay directory the endpoint polls

Provenance labeling (explicit, not forged): the real envelope has no
"type" field, so every received item is labeled type "m2m.message":
an ordinary verified message. The label confers NO execution
authority and must never be read as sender-authenticated turn
intent. Execution is conferred solely by the operator's
METIS_M2M_SENDERS + METIS_M2M_TURN_TYPES allowlists, enforced by
the bridge, not here.

Trust chain per item (all must hold, else the item is skipped and
reported on stderr, never emitted):
  1. The real relay_poll() delivered it: signature verified against
     the enrolled contact key, expiry pruned, replay deduped,
     unenrolled senders dropped. Poll writes the local inbox file,
     posts the signed ack, and marks seen -- all inside m2m.py.
  2. The sender_fp equals the enrolled fingerprint for the contact
     name poll reported (binds the human-readable name to the
     verified key). Poll-direct items cross-check the pre-read
     header; recovered items use the durable contacts map against
     the durable inbox file.
  3. expires_at comes from the signature-verified envelope header,
     formatted RFC3339 -- or is empty for recovered items, in which
     case the bridge bounds executability to receipt time plus the
     operator's METIS_M2M_RECEIPT_TTL. The adapter never fabricates
     a sender TTL.

Recovery (at-least-once, never lost): poll durably consumes a
message (inbox + signed ack + seen + queue delete) before Metis is
guaranteed to claim it. A crash between consume and commit, an
arrival after the header snapshot, or a parent crash after this
process flushed stdout but before Metis committed the dispatch
would strand a verified message. The ONLY skip-set is the
parent-confirmed ack file (m2m_acked.json in the endpoint home),
which Metis appends to after it durably claims the dispatch AND
records its decision; this run records nothing itself. Every poll
also sweeps the durable local inbox for delivered-but-unconfirmed
items, emitting them receipt-bounded. Dispatch idempotency on the
Metis side (same message_id converges on the same command) makes
re-emission safe. There is deliberately NO point at which an item
is marked done but never committed -- a self-recorded cursor did
exactly that and stranded messages when the parent died after the
flush but before the commit. _M2M_DRIVE_CRASH_AFTER_POLL=1 is a
test-only hook that exits 99 right after poll returns, to prove
the next run recovers. _M2M_DRIVE_CRASH_BEFORE_RECORD=1 exits 98
after the print+flush, to prove the next run re-emits and the
Metis side converges once the parent confirms.

A non-empty stdout is always a JSON array (receive) or object (ack);
diagnostics go to stderr so parsers never see them.
"""

import base64
import contextlib
import datetime
import io
import json
import os
import sys
from pathlib import Path

ADAPTER_TYPE = "m2m.message"

EMIT_CURSOR = "metis_emit.json"  # legacy, no longer consulted (see below)
ACK_CURSOR = "m2m_acked.json"


def load_acked(home):
    """Parent-confirmed message IDs — the ONLY skip-set.

    Metis appends an ID here after it has durably claimed the
    dispatch AND recorded its decision, so anything absent was
    never committed and must be re-emitted. The legacy
    self-recorded emit file is deliberately ignored: a run that
    recorded it but whose parent died before claiming stranded
    those messages, and honoring it would re-strand them.
    """
    try:
        data = json.loads((home / ACK_CURSOR).read_text())
        return set(data.get("acked", []))
    except Exception:
        return set()


def fail(msg):
    print(f"m2m_drive: {msg}", file=sys.stderr)
    sys.exit(1)


def load_client():
    lib = os.environ.get("METIS_M2M_LIB", "")
    home = os.environ.get("METIS_M2M_HOME", "")
    relay = os.environ.get("METIS_M2M_RELAY", "")
    if not lib or not home or not relay:
        fail("need METIS_M2M_LIB, METIS_M2M_HOME, METIS_M2M_RELAY in the environment")
    sys.path.insert(0, lib)
    try:
        import m2m as client
    except Exception as e:
        fail(f"cannot import real client from {lib}: {e}")
    return client, Path(home), Path(relay)


def snapshot_headers(relay, own_fp):
    """Copy envelope headers currently queued for us (metadata only).

    No trust is placed here: trust comes from relay_poll() delivery
    plus the sender_fp cross-check below. Returns msg_id -> header.
    """
    box = relay / own_fp
    out = {}
    if not box.is_dir():
        return out
    for f in sorted(box.glob("*.json")):
        try:
            env = json.loads(f.read_text())
            h = env.get("header", {})
            if isinstance(h.get("msg_id"), str):
                out[h["msg_id"]] = h
        except Exception as e:
            print(f"m2m_drive: unreadable queue file {f.name}: {e}", file=sys.stderr)
    return out


def emit_item(mid, sender_fp, exp_str, text):
    return {
        "message_id": mid,
        "sender_fp": sender_fp,
        "type": ADAPTER_TYPE,
        "expires_at": exp_str,
        "payload_b64": base64.b64encode(text.encode("utf-8")).decode(),
    }


def cmd_receive(client, home, relay):
    ep = client.Endpoint(home)
    try:
        data, _, _ = ep.load()
    except SystemExit as e:
        fail(f"no endpoint identity: {e}")
    own_fp = data["fingerprint"]
    headers = snapshot_headers(relay, own_fp)
    contacts = json.loads(ep.contacts.read_text()) if ep.contacts.exists() else {}
    fp_of_name = {c.get("name"): c.get("fingerprint") for c in contacts.values()}
    # The ONLY skip-set is the parent-confirmed ack file. This run
    # never records anything itself: anything it hands to stdout
    # but the parent fails to commit is re-emitted next run, and
    # Metis dispatch idempotency converges the redelivery.
    emitted = load_acked(home)
    # relay_poll prints DROP/REJECT chatter and per-message JSON to
    # stdout; silence it -- the return value is the verified result.
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        delivered = client.relay_poll(relay, ep, None)
    if os.environ.get("_M2M_DRIVE_CRASH_AFTER_POLL") == "1":
        # Test-only hook: simulate a crash after the client durably
        # consumed the messages but before anything was emitted.
        sys.exit(99)
    items = []
    for m in delivered:
        mid = m.get("msg_id")
        name = m.get("from")
        text = m.get("plaintext", "")
        if mid in emitted:
            continue
        fp = fp_of_name.get(name)
        if fp is None:
            print(f"m2m_drive: msg {mid} from unknown contact {name!r}; not emitted", file=sys.stderr)
            continue
        h = headers.get(mid)
        if h is not None and h.get("sender_fp") != fp:
            print(f"m2m_drive: msg {mid} sender/name mismatch; kept as data, not emitted", file=sys.stderr)
            continue
        if not text:
            print(f"m2m_drive: msg {mid} carries empty plaintext; not emitted", file=sys.stderr)
            continue
        if h is None:
            # Consumed-but-never-emitted (arrival after the snapshot
            # or a prior crash): recover from the durable inbox with
            # NO sender expiry -- the bridge bounds it to receipt.
            items.append(emit_item(mid, fp, "", text))
            print(f"m2m_drive: msg {mid} recovered from durable inbox (receipt-bounded)", file=sys.stderr)
        else:
            try:
                exp = datetime.datetime.fromtimestamp(int(h["expires_at"]), datetime.timezone.utc)
            except Exception:
                print(f"m2m_drive: msg {mid} has unusable expires_at; not emitted", file=sys.stderr)
                continue
            items.append(emit_item(mid, fp, exp.strftime("%Y-%m-%dT%H:%M:%SZ"), text))
        emitted.add(mid)
    # Sweep the durable local inbox for delivered-but-never-emitted
    # items this run did not see (prior crash between consume and
    # emit, or a poll return the snapshot missed). Same receipt-
    # bounded recovery; dispatch idempotency downstream makes this
    # safe against double emission.
    box = ep.inbox if hasattr(ep, "inbox") else home / "inbox"
    if box.is_dir():
        for f in sorted(box.glob("*.json")):
            try:
                local = json.loads(f.read_text())
            except Exception:
                continue
            mid = local.get("msg_id")
            if not mid or mid in emitted:
                continue
            name = local.get("from")
            text = local.get("plaintext", "")
            fp = fp_of_name.get(name)
            if fp is None or not text:
                continue
            items.append(emit_item(mid, fp, "", text))
            print(f"m2m_drive: msg {mid} recovered from durable inbox (receipt-bounded)", file=sys.stderr)
            emitted.add(mid)
    # The run records nothing: the parent confirms by appending to
    # the ack file only after it durably commits the dispatch, so a
    # crash anywhere before that — including after this flush —
    # redelivers next run instead of stranding the message.
    print(json.dumps(items), flush=True)
    if os.environ.get("_M2M_DRIVE_CRASH_BEFORE_RECORD") == "1":
        # Test-only hook: simulate a crash after the stdout handoff,
        # before the parent could commit. The next run must re-emit.
        sys.exit(98)


def cmd_ack(client, home, relay, argv):
    if len(argv) != 4:
        fail("usage: ack <message_id> <state>")
    mid, state = argv[2], argv[3]
    ep = client.Endpoint(home)
    # The sender-visible acknowledgment is the signed ack poll posted
    # into the relay's ack box for the SENDER (acks/<sender_fp>/):
    # that is the file the sender's ack-status reads. Confirm it is
    # present and verifies with the client's own verifier against our
    # enrolled identity before Metis records the state. No new crypto
    # here: client.verify_ack is the audited check (audit A1).
    try:
        me = ep.export_bundle()
    except SystemExit as e:
        fail(f"no endpoint identity: {e}")
    found = None
    acks_root = relay / "acks"
    if acks_root.is_dir():
        for f in sorted(acks_root.glob(f"*/{mid}.json")):
            try:
                found = json.loads(f.read_text())
                break
            except Exception:
                continue
    if not found or not client.verify_ack(found, me):
        fail(f"no sender-visible signed ack for {mid}; refusing to record {state}")
    # Exact message-ID binding: the ack body must name the requested
    # message. verify_ack checks internal consistency and the
    # signature, but the binding to THIS ack call is the adapter's
    # job -- a valid ack for another message must not satisfy it.
    if found.get("body", {}).get("msg_id") != mid:
        fail(f"signed ack binds {found.get('body', {}).get('msg_id')!r}, not {mid}; refusing to record {state}")
    # Transport vs execution, kept distinct: this echo confirms the
    # sender-visible SIGNED TRANSPORT acknowledgment exists. It says
    # nothing about Muse execution -- execution state lives in
    # Metis's dispatch ledger, never in this echo.
    print(json.dumps({"message_id": mid, "state": state}))


def main(argv):
    if len(argv) < 2:
        fail("usage: receive | ack <message_id> <state>")
    client, home, relay = load_client()
    if argv[1] == "receive" and len(argv) == 2:
        cmd_receive(client, home, relay)
    elif argv[1] == "ack":
        cmd_ack(client, home, relay, argv)
    else:
        fail(f"unknown subcommand {argv[1]!r}: want receive | ack")


if __name__ == "__main__":
    main(sys.argv)
