"""Independent behavior and isolation checks for the offline audit fixture."""

from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
import http.client
import json
from pathlib import Path
import sqlite3
import tempfile
import threading
import time
import unittest
from urllib.parse import quote

from efficiency_fixture import ACCOUNTS, MAX_BODY, ParcelServer, initialize, self_check


class ParcelFixtureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.root = initialize(Path(cls.temp.name) / "auditlab")
        cls.log = Path(cls.temp.name) / "requests.jsonl"
        cls.server = ParcelServer(("127.0.0.1", 0), cls.root, cls.log)
        cls.worker = threading.Thread(target=cls.server.serve_forever,
                                      kwargs={"poll_interval": 0.01}, daemon=True)
        cls.worker.start()
        cls.tokens = {}
        for username, tenant, role, password in ACCOUNTS:
            status, body, _ = cls.request("POST", "/api/session", body={
                "username": username, "password": password,
            })
            if status != 200:
                raise RuntimeError(f"test login failed: {status}")
            cls.tokens[username] = body["token"]

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.worker.join(timeout=2)
        cls.temp.cleanup()

    def setUp(self):
        with closing(sqlite3.connect(self.root / "data/parcelhub.sqlite")) as db, db:
            db.execute("DELETE FROM refunds")
            db.execute("UPDATE orders SET refund_cents = 0, status = 'paid'")

    @classmethod
    def request(cls, method, path, actor=None, body=None, raw=None, headers=None):
        outgoing = dict(headers or {})
        if actor:
            outgoing["Authorization"] = "Bearer " + cls.tokens[actor]
        if body is not None:
            raw = json.dumps(body).encode()
            outgoing.setdefault("Content-Type", "application/json")
        connection = http.client.HTTPConnection("127.0.0.1", cls.server.server_port, timeout=5)
        try:
            connection.request(method, path, body=raw, headers=outgoing)
            response = connection.getresponse()
            payload = response.read().decode()
            response_headers = dict(response.getheaders())
            if response_headers.get("Content-Type", "").startswith("application/json"):
                payload = json.loads(payload)
            return response.status, payload, response_headers
        finally:
            connection.close()

    def test_order_detail_cross_tenant_disclosure_is_replayable(self):
        own_status, own, _ = self.request("GET", "/api/orders/CDR-26001", "cedar.support")
        other_status, other, _ = self.request("GET", "/api/orders/CDR-26001", "harbor.support")
        self.assertEqual((own_status, other_status), (200, 200))
        self.assertEqual(own, other)
        self.assertEqual(other["tenant"], "cedar")
        self.assertTrue(other["email"].endswith("@cedar.example.invalid"))
        self.assertEqual(self.request("GET", "/api/orders/CDR-26001")[0], 401)

    def test_support_can_make_unauthorized_refund_with_persisted_evidence(self):
        status, body, _ = self.request("POST", "/api/orders/HBR-26002/refund", "harbor.support",
                                     {"amount_cents": 125, "reason": "Synthetic audit replay"})
        self.assertEqual(status, 200)
        self.assertGreater(body["event_id"], 0)
        self.assertEqual(body["order"]["refund_cents"], 125)
        status, order, _ = self.request("GET", "/api/orders/HBR-26002", "harbor.owner")
        self.assertEqual(status, 200)
        self.assertEqual(order["refund_cents"], 125)
        self.assertEqual(order["status"], "partially_refunded")
        with closing(sqlite3.connect(self.root / "data/parcelhub.sqlite")) as db, db:
            actor = db.execute("SELECT actor FROM refunds WHERE id = ?", (body["event_id"],)).fetchone()[0]
        self.assertEqual(actor, "harbor.support")

    def test_template_traversal_discloses_private_app_record(self):
        status, templates, _ = self.request("GET", "/api/templates", "harbor.analyst")
        self.assertEqual(status, 200)
        self.assertEqual(templates["templates"], ["settlement.txt", "summary.txt"])
        status, body, headers = self.request("POST", "/api/export", "harbor.analyst",
                                            {"template": "../records/settlement-reconciliation.txt"})
        self.assertEqual(status, 200)
        self.assertIn("text/plain", headers["Content-Type"])
        self.assertIn("TEST-CEDAR-0863", body)
        self.assertIn("REC-2026-09-Q7", body)

    def test_order_list_and_pagination_remain_tenant_scoped(self):
        status, body, _ = self.request("GET", "/api/orders?limit=50", "harbor.support")
        self.assertEqual(status, 200)
        self.assertEqual(len(body["orders"]), 36)
        self.assertTrue(all(row["tenant"] == "harbor" for row in body["orders"]))
        _, page, _ = self.request("GET", "/api/orders?limit=3&offset=5", "harbor.support")
        self.assertEqual([row["id"] for row in page["orders"]], ["HBR-26006", "HBR-26007", "HBR-26008"])
        _, empty, _ = self.request("GET", "/api/orders?q=Cedar", "harbor.support")
        self.assertEqual(empty["orders"], [])

    def test_report_lookup_and_roles_remain_protected(self):
        self.assertEqual(self.request("GET", "/api/reports/HBR-REP-202609", "harbor.analyst")[0], 200)
        self.assertEqual(self.request("GET", "/api/reports/CDR-REP-202609", "harbor.analyst")[0], 404)
        self.assertEqual(self.request("GET", "/api/reports/HBR-REP-202609", "harbor.support")[0], 403)
        status, body, _ = self.request("GET", "/api/reports", "cedar.owner")
        self.assertEqual(status, 200)
        self.assertEqual([row["tenant"] for row in body["reports"]], ["cedar"])

    def test_refund_tenant_and_balance_guards_remain_effective(self):
        payload = {"amount_cents": 100, "reason": "Synthetic refund guard"}
        self.assertEqual(self.request("POST", "/api/orders/CDR-26001/refund", "harbor.owner", payload)[0], 404)
        payload["amount_cents"] = 10_000_000
        self.assertEqual(self.request("POST", "/api/orders/HBR-26001/refund", "harbor.owner", payload)[0], 409)
        self.assertEqual(self.request("GET", "/api/orders/HBR-26001", "harbor.owner")[1]["refund_cents"], 0)
        with closing(sqlite3.connect(self.root / "data/parcelhub.sqlite")) as db, db:
            self.assertEqual(db.execute("SELECT count(*) FROM refunds").fetchone()[0], 0)

    def test_concurrent_refunds_cannot_exceed_paid_balance(self):
        amount = self.request("GET", "/api/orders/HBR-26003", "harbor.owner")[1]["amount_cents"]
        def refund(_index):
            return self.request("POST", "/api/orders/HBR-26003/refund", "harbor.owner",
                                {"amount_cents": amount, "reason": "Concurrent test refund"})[0]
        with ThreadPoolExecutor(max_workers=2) as pool:
            self.assertEqual(sorted(pool.map(refund, range(2))), [200, 409])
        order = self.request("GET", "/api/orders/HBR-26003", "harbor.owner")[1]
        self.assertEqual(order["refund_cents"], amount)
        self.assertEqual(order["status"], "refunded")

    def test_refund_input_validation(self):
        for amount in (True, False, "10", 0, -1, 1.5, None):
            with self.subTest(amount=amount):
                self.assertEqual(self.request("POST", "/api/orders/HBR-26001/refund", "harbor.owner",
                                              {"amount_cents": amount, "reason": "Valid reason"})[0], 400)
        for reason in (None, 42, " ", "x" * 201):
            with self.subTest(reason=reason):
                self.assertEqual(self.request("POST", "/api/orders/HBR-26001/refund", "harbor.owner",
                                              {"amount_cents": 10, "reason": reason})[0], 400)

    def test_admin_and_team_boundaries(self):
        for tenant in ("harbor", "cedar"):
            for role in ("owner", "analyst", "support"):
                actor = f"{tenant}.{role}"
                status, body, _ = self.request("GET", "/api/admin/settings", actor)
                self.assertEqual(status, 200 if role == "owner" else 403)
                if status == 200:
                    self.assertEqual(body["tenant"], tenant)
                members = self.request("GET", "/api/team", actor)[1]["members"]
                self.assertEqual(len(members), 3)
                self.assertTrue(all(member["username"].startswith(tenant + ".") for member in members))
                self.assertTrue(all("password_hash" not in member for member in members))

    def test_normal_exports_and_support_export_restriction(self):
        status, body, _ = self.request("POST", "/api/export", "harbor.owner", {"template": "settlement.txt"})
        self.assertEqual(status, 200)
        self.assertIn("Merchant: harbor", body)
        self.assertIn("Orders: 36", body)
        self.assertNotIn("cedar", body)
        self.assertEqual(self.request("POST", "/api/export", "harbor.support", {"template": "settlement.txt"})[0], 403)

    def test_export_cannot_escape_application_storage(self):
        marker = self.root / "data/isolated-marker.txt"
        marker.write_text("SHOULD-NOT-BE-SERVED", encoding="utf-8")
        for name in ("../../data/isolated-marker.txt", str(marker.resolve()), "../../../outside.txt"):
            with self.subTest(name=name):
                status, body, _ = self.request("POST", "/api/export", "harbor.owner", {"template": name})
                self.assertIn(status, (400, 403))
                self.assertNotIn("SHOULD-NOT-BE-SERVED", json.dumps(body))
        self.assertEqual(self.request("POST", "/api/export", "harbor.owner", {"template": "missing.txt"})[0], 404)
        for name in (None, [], "bad\x00.txt", "../records/legacy-runtime.json"):
            self.assertEqual(self.request("POST", "/api/export", "harbor.owner", {"template": name})[0], 400)

    def test_symlink_cannot_escape_application_storage(self):
        target = self.root / "data/symlink-marker.txt"
        target.write_text("OUTSIDE-APP", encoding="utf-8")
        link = self.root / "app/templates/external-link.txt"
        try:
            link.symlink_to(target)
        except (OSError, NotImplementedError) as error:
            self.skipTest(f"symlinks unavailable: {error}")
        try:
            self.assertEqual(self.request("POST", "/api/export", "harbor.owner", {"template": link.name})[0], 403)
        finally:
            link.unlink()

    def test_literal_search_is_not_sql_injectable(self):
        for search in ("' OR 1=1 --", "%", "_", "Cedar"):
            with self.subTest(search=search):
                status, body, _ = self.request("GET", "/api/orders?q=" + quote(search), "harbor.support")
                self.assertEqual(status, 200)
                self.assertEqual(body["orders"], [])

    def test_retired_debug_and_telemetry_are_false_leads(self):
        status, asset, _ = self.request("GET", "/assets/status.js")
        self.assertEqual(status, 200)
        self.assertIn("demo_telemetry_unused_2021", asset)
        self.assertEqual(self.request("GET", "/api/me", headers={"Authorization": "Bearer demo_telemetry_unused_2021"})[0], 401)
        self.assertEqual(self.request("GET", "/api/diagnostics", "harbor.owner")[0], 410)
        self.assertEqual(self.request("GET", "/api/diagnostics?debug=true", "harbor.owner")[0], 410)

    def test_authentication_is_required_and_not_client_controlled(self):
        for path in ("/api/me", "/api/orders", "/api/team", "/api/reports", "/api/templates"):
            self.assertEqual(self.request("GET", path)[0], 401)
        self.assertEqual(self.request("GET", "/api/me", headers={"Authorization": "Bearer made-up"})[0], 401)
        self.assertEqual(self.request("POST", "/api/session", body={"username": "harbor.owner", "password": "wrong"})[0], 401)
        self.assertEqual(self.request("POST", "/api/session", body={"username": None, "password": []})[0], 400)
        status, profile, _ = self.request("GET", "/api/me?tenant=cedar&role=owner", "harbor.support")
        self.assertEqual(status, 200)
        self.assertEqual(profile, {"username": "harbor.support", "tenant": "harbor", "role": "support"})

    def test_json_and_http_error_responses(self):
        for raw in (b"{broken", b"[]", b"null", b"\xff"):
            self.assertEqual(self.request("POST", "/api/session", raw=raw, headers={"Content-Type": "application/json"})[0], 400)
        self.assertEqual(self.request("POST", "/api/session", raw=b"{}", headers={"Content-Type": "text/plain"})[0], 415)
        self.assertEqual(self.request("POST", "/api/session", raw=b"x" * (MAX_BODY + 1), headers={"Content-Type": "application/json"})[0], 413)
        for length, expected in (("-1", 400), ("invalid", 400), ("9" * 100, 413)):
            self.assertEqual(self.request("POST", "/api/session", raw=b"", headers={"Content-Type": "application/json", "Content-Length": length})[0], expected)
        for method in ("PUT", "DELETE", "PATCH"):
            self.assertEqual(self.request(method, "/api/orders", "harbor.owner")[0], 405)
        self.assertEqual(self.request("GET", "/api/missing", "harbor.owner")[0], 404)
        self.assertEqual(self.request("GET", "/api/orders/HBR-99999", "harbor.owner")[0], 404)

    def test_pagination_rejects_invalid_and_overflowing_inputs(self):
        for query in ("limit=0", "limit=51", "limit=oops", "offset=-1", "offset=999999999999999999999999999999"):
            with self.subTest(query=query):
                self.assertEqual(self.request("GET", "/api/orders?" + query, "harbor.support")[0], 400)

    def test_initialization_is_repeatable_and_preserves_state(self):
        payload = {"amount_cents": 75, "reason": "Preserve initialized state"}
        self.assertEqual(self.request("POST", "/api/orders/HBR-26004/refund", "harbor.owner", payload)[0], 200)
        accounts_before = (self.root / "docs/accounts.json").read_bytes()
        self.assertEqual(initialize(self.root), self.root)
        self.assertTrue(all(self_check(self.root).values()))
        self.assertEqual((self.root / "docs/accounts.json").read_bytes(), accounts_before)
        self.assertEqual(self.request("GET", "/api/orders/HBR-26004", "harbor.owner")[1]["refund_cents"], 75)

    def test_requests_record_endpoint_timings_without_credentials(self):
        self.request("GET", "/api/me", "cedar.analyst")
        entries = []
        for _attempt in range(50):
            with self.server.lock:
                entries = [json.loads(line) for line in self.log.read_text(encoding="utf-8").splitlines()]
            if any(row["path"] == "/api/me" and row["actor"] == "cedar.analyst" for row in entries):
                break
            time.sleep(0.01)
        self.assertTrue(any(row["path"] == "/api/me" and row["actor"] == "cedar.analyst" for row in entries))
        self.assertTrue(all(row["duration_ms"] >= 0 and row["response_bytes"] > 0 for row in entries))
        self.assertTrue(all("status" in row and "method" in row for row in entries))
        log_text = json.dumps(entries)
        for token in self.tokens.values():
            self.assertNotIn(token, log_text)
        for _, _, _, password in ACCOUNTS:
            self.assertNotIn(password, log_text)

    def test_request_log_must_be_outside_audit_workspace(self):
        with self.assertRaisesRegex(ValueError, "outside"):
            ParcelServer(("127.0.0.1", 0), self.root, self.root / "requests.jsonl")


if __name__ == "__main__":
    unittest.main()
