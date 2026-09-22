import unittest

from tools.rfe_rig import mcp_client


class UrlPolicyTest(unittest.TestCase):
    """The bearer token travels only over https, or over http to loopback."""

    def test_https_and_loopback_accepted(self):
        for url in ("https://rig.example.org/mcp", "http://127.0.0.1:8791/mcp",
                    "http://localhost:8791/mcp", "http://[::1]:8791/mcp", "http://127.0.0.2/mcp"):
            self.assertEqual(mcp_client.check_url(url), url)

    def test_cleartext_to_a_remote_host_refused(self):
        for url in ("http://10.0.0.5:8791/mcp", "http://q2e.example.org/mcp", "ftp://127.0.0.1/x"):
            with self.assertRaises(SystemExit):
                mcp_client.check_url(url)
        with self.assertRaises(SystemExit):
            mcp_client.MCP("http://10.0.0.5:8791/mcp", "t")


class UnpackTest(unittest.TestCase):
    """A failed tool call is an error, never an answer to score."""

    def test_structured_answer(self):
        r = {"result": {"structuredContent": {"answer": "Ответ.", "citations": [1]},
                        "content": [{"type": "text", "text": "Ответ."}]}}
        answer, dt, err, sc = mcp_client._unpack(r, 1.0)
        self.assertEqual((answer, err, sc["citations"]), ("Ответ.", None, [1]))

    def test_text_content_fallback(self):
        r = {"result": {"content": [{"type": "text", "text": "plain"}]}}
        self.assertEqual(mcp_client._unpack(r, 0.0)[0], "plain")

    def test_is_error_result(self):
        r = {"result": {"isError": True,
                        "content": [{"type": "text", "text": "generator unavailable"}]}}
        answer, _dt, err, sc = mcp_client._unpack(r, 0.0)
        self.assertEqual(answer, "")
        self.assertEqual(err["message"], "generator unavailable")
        self.assertEqual(sc, {})

    def test_jsonrpc_error(self):
        r = {"error": {"code": -32000, "message": "boom"}}
        answer, _dt, err, _sc = mcp_client._unpack(r, 0.0)
        self.assertEqual(answer, "")
        self.assertEqual(err["message"], "boom")

    def test_parse_body_sse(self):
        raw = "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
        self.assertEqual(mcp_client.parse_body(raw)["id"], 1)
        self.assertEqual(mcp_client.parse_body(""), {})


if __name__ == "__main__":
    unittest.main()
