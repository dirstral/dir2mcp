"""Tests for the checker functions of ``release_smoke`` (issue #1101).

Run via ``make test-release-tools`` (preferred), or directly:

    cd scripts && python3 -m unittest test_release_smoke

The ``cd`` is required because this file imports ``release_smoke`` as a
sibling module. No test opens a network connection or starts a daemon: the
checkers take plain values, and the open_file check takes the tool call as a
function argument.
"""

from __future__ import annotations

import unittest

from release_smoke import (
    abstention_reason,
    ask_verdict,
    is_pdf_source,
    is_texty,
    list_all,
    open_file_verdict,
    summary_line,
)

# The shape of what open_file returned for a skipped PDF in the v0.11.4
# release-candidate gate (#1100): uncompressed PDF syntax, mostly letters.
RAW_PDF_SOURCE = (
    "%PDF-1.4\n"
    "1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n"
    "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n"
    "3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
    "/Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n"
    "4 0 obj\n<< /Length 44 >>\nstream\n"
    "BT /F1 12 Tf 72 712 Td (Financial Investigation Agency Act) Tj ET\n"
    "endstream\nendobj\n"
)

# The same source with the header cut off, as a page slice could return it.
RAW_PDF_BODY_NO_HEADER = RAW_PDF_SOURCE.split("\n", 1)[1]

# The cross-reference table alone, with no object markers.
RAW_PDF_XREF = (
    "trailer << /Size 6 /Root 1 0 R >>\n"
    "xref\n0 6\n0000000000 65535 f \n0000000009 00000 n \n"
    "startxref\n1234\n%%EOF\n"
)

REAL_PAGE_TEXT = (
    "FINANCIAL INVESTIGATION AGENCY ACT, 2003\n\n"
    "Section 4. Functions of the Agency. The Agency shall receive, obtain, "
    "investigate, analyse and disseminate information which relates or may "
    "relate to a financial offence or the proceeds of a financial offence. "
    "The Agency may request information from any person who is required to "
    "make a suspicious transaction report, and it may end an inquiry once "
    "the objects of the inquiry are met."
)

GROUNDED_ANSWER = (
    "The Financial Investigation Agency can receive, analyse and disseminate "
    "reports of suspicious transactions, and it can request information from "
    "reporting entities [laws/fia-act.pdf] (section 4)."
)

ABSTAINING_ANSWER = (
    "Based on the provided context, there is no information about the powers "
    "of the BVI Financial Investigation Agency."
)


def _cites(n):
    """Return n citation objects."""
    return [{"rel_path": f"doc{i}.pdf"} for i in range(n)]


def _lister(files):
    """Return a list_files tool result that holds the given file rows."""
    return {"isError": False, "structuredContent": {"files": files}}


def _row(rel_path, status="ok", deleted=False):
    """Return one list_files row."""
    return {"rel_path": rel_path, "status": status, "deleted": deleted}


class IsTextyTest(unittest.TestCase):
    """is_texty and is_pdf_source: extracted text against raw PDF syntax."""

    def test_raw_pdf_source_is_rejected(self):
        """The #1100 payload (header plus objects) is not extracted text."""
        self.assertFalse(is_texty(RAW_PDF_SOURCE))

    def test_raw_pdf_body_without_header_is_rejected(self):
        """Object markers alone are enough to reject the text."""
        self.assertFalse(is_texty(RAW_PDF_BODY_NO_HEADER))

    def test_xref_table_is_rejected(self):
        """A cross-reference table is PDF syntax."""
        self.assertTrue(is_pdf_source(RAW_PDF_XREF))

    def test_real_page_text_is_accepted(self):
        """A real page of extracted prose passes."""
        self.assertTrue(is_texty(REAL_PAGE_TEXT))
        self.assertFalse(is_pdf_source(REAL_PAGE_TEXT))

    def test_prose_that_names_the_words_is_accepted(self):
        """The words xref and endobj inside a sentence are not markers."""
        text = ("The parser reads the xref section and stops at each endobj "
                "keyword, which this guide describes in plain words.")
        self.assertTrue(is_texty(text))

    def test_short_or_empty_text_is_rejected(self):
        """Empty, None and very short text fail."""
        self.assertFalse(is_texty(""))
        self.assertFalse(is_texty(None))
        self.assertFalse(is_texty("too short"))


class OpenFileVerdictTest(unittest.TestCase):
    """open_file_verdict: PASS, FAIL or SKIPPED from list_files and open_file."""

    def test_no_pdf_in_corpus_is_skipped(self):
        """A corpus with no PDF gives SKIPPED, never PASS."""
        status, detail = open_file_verdict(_lister([]), lambda rp: REAL_PAGE_TEXT)
        self.assertEqual(status, "SKIPPED")
        self.assertIn("no PDF", detail)

    def test_list_files_error_fails_and_does_not_skip(self):
        """A list_files error is a FAIL: an absent list is not an empty corpus."""
        status, _ = open_file_verdict({"isError": True}, lambda rp: REAL_PAGE_TEXT)
        self.assertEqual(status, "FAIL")

    def test_pdfs_that_were_not_extracted_fail(self):
        """PDFs that exist but have no status ok fail without an open_file call."""
        rows = [_row("a.pdf", "skipped"), _row("b.pdf", "error")]
        called = []
        status, detail = open_file_verdict(_lister(rows), called.append)
        self.assertEqual(status, "FAIL")
        self.assertIn("none extracted", detail)
        self.assertEqual(called, [], "open_file must not run on an unextracted PDF")

    def test_only_extracted_pdfs_are_opened(self):
        """Skipped and deleted PDFs are not probed."""
        rows = [_row("skipped.pdf", "skipped"), _row("gone.pdf", deleted=True),
                _row("good.pdf")]
        called = []

        def opener(rp):
            called.append(rp)
            return REAL_PAGE_TEXT

        status, _ = open_file_verdict(_lister(rows), opener)
        self.assertEqual(status, "PASS")
        self.assertEqual(called, ["good.pdf"])

    def test_raw_pdf_source_from_open_file_fails(self):
        """open_file text that is PDF syntax fails and says why."""
        status, detail = open_file_verdict(_lister([_row("a.pdf")]), lambda rp: RAW_PDF_SOURCE)
        self.assertEqual(status, "FAIL")
        self.assertIn("PDF syntax", detail)

    def test_open_file_error_fails(self):
        """An open_file error (opener returns None) fails."""
        status, detail = open_file_verdict(_lister([_row("a.pdf")]), lambda rp: None)
        self.assertEqual(status, "FAIL")
        self.assertIn("tool error", detail)


class AskVerdictTest(unittest.TestCase):
    """ask_verdict and abstention_reason: grounded answers against abstentions."""

    def test_not_enough_information_with_citations_fails(self):
        """A "not enough information" answer with citations fails."""
        for answer in (
            "I don't have enough information to answer this question.",
            "I do not have sufficient information in the provided context.",
            "There is not enough information in the documents to say.",
            "The sources give insufficient information about this.",
        ):
            with self.subTest(answer=answer):
                ok, detail = ask_verdict({"answer": answer, "citations": _cites(3)})
                self.assertFalse(ok)
                self.assertIn("abstained", detail)

    def test_grounded_answer_with_citations_passes(self):
        """A grounded answer with citations passes."""
        ok, detail = ask_verdict({"answer": GROUNDED_ANSWER, "citations": _cites(2)})
        self.assertTrue(ok, detail)

    def test_abstaining_answer_with_citations_fails(self):
        """The #1101 comment case: an abstention with 8 citations fails."""
        ok, detail = ask_verdict({"answer": ABSTAINING_ANSWER, "citations": _cites(8)})
        self.assertFalse(ok)
        self.assertIn("abstained", detail)

    def test_answer_without_citations_fails(self):
        """An answer with no citation fails."""
        ok, _ = ask_verdict({"answer": GROUNDED_ANSWER, "citations": []})
        self.assertFalse(ok)

    def test_server_refusals_are_abstentions(self):
        """The fixed refusal texts of internal/retrieval are abstentions."""
        for answer in (
            "Insufficient evidence to answer: retrieval returned 3 candidate passages.",
            "I could not verify the answer against the retrieved passages, so I am not reporting it.",
            "No relevant context found in the indexed corpus.",
        ):
            with self.subTest(answer=answer):
                self.assertIsNotNone(abstention_reason({"answer": answer}))

    def test_model_abstention_forms(self):
        """Common model wordings for a no-information answer are abstentions."""
        for answer in (
            "The provided context does not contain information about this act.",
            "The documents do not mention the reporting deadline.",
            "There is no information in the context about that section.",
            "I cannot answer this question from the provided documents.",
        ):
            with self.subTest(answer=answer):
                self.assertIsNotNone(abstention_reason({"answer": answer}))

    def test_structured_abstention_fields(self):
        """evidence=insufficient and faithfulness=unsupported are abstentions."""
        self.assertIsNotNone(abstention_reason(
            {"answer": GROUNDED_ANSWER, "evidence": "insufficient"}))
        self.assertIsNotNone(abstention_reason(
            {"answer": GROUNDED_ANSWER, "faithfulness": "unsupported"}))
        self.assertIsNone(abstention_reason(
            {"answer": GROUNDED_ANSWER, "evidence": "strong", "faithfulness": "verified"}))

    def test_late_caveat_in_a_grounded_answer_passes(self):
        """Only the opening of the answer is read for model phrases."""
        answer = (GROUNDED_ANSWER + " " + ("Section 4 lists the duties in detail. " * 10)
                  + "The act gives no information about penalties.")
        self.assertIsNone(abstention_reason({"answer": answer}))


class SummaryLineTest(unittest.TestCase):
    """summary_line: ALL PASS only when nothing failed and nothing was skipped."""

    def test_all_pass_only_without_skips(self):
        """No fails and no skips gives ALL PASS."""
        self.assertIn("ALL PASS", summary_line([], []))

    def test_skip_is_not_all_pass(self):
        """A skip gives PASS with N SKIPPED, not ALL PASS."""
        line = summary_line([], ["open_file page=1 returns text"])
        self.assertNotIn("ALL PASS", line)
        self.assertIn("PASS with 1 SKIPPED", line)

    def test_fail_reports_fails_and_skips(self):
        """A fail names the failed checks and counts the skips."""
        line = summary_line(["stats: errors==0"], ["open_file page=1 returns text"])
        self.assertIn("FAILED", line)
        self.assertIn("stats: errors==0", line)
        self.assertIn("1 SKIPPED", line)



class ListAllTest(unittest.TestCase):
    """list_all reads every page of a list_files listing."""

    def _pager(self, rows, total=None):
        """Return a call function that pages rows like list_files, and the
        list of the arguments it got."""
        calls = []

        def call(args):
            calls.append(args)
            off, lim = args["offset"], args["limit"]
            return {"isError": False, "structuredContent": {
                "files": rows[off:off + lim], "limit": lim, "offset": off,
                "total": len(rows) if total is None else total}}
        return call, calls

    def test_extracted_pdf_after_first_page_is_found(self):
        """An extracted PDF after 600 skipped ones reaches open_file_verdict."""
        rows = [_row(f"a/{i:04d}.pdf", status="skipped") for i in range(600)]
        rows.append(_row("z/real.pdf"))
        call, calls = self._pager(rows)
        lf = list_all(call, {"glob": "**/*.pdf"}, lambda r: True)
        self.assertEqual(len(lf["structuredContent"]["files"]), 601)
        self.assertEqual(len(calls), 2)
        status, _ = open_file_verdict(lf, lambda rp: GROUNDED_ANSWER)
        self.assertEqual(status, "PASS")

    def test_error_page_is_returned(self):
        """A tool error on a later page is returned as is."""
        rows = [_row(f"{i}.pdf") for i in range(700)]
        ok_call, _ = self._pager(rows)

        def call(args):
            if args["offset"] > 0:
                return {"isError": True}
            return ok_call(args)
        self.assertTrue(list_all(call, {}, lambda r: True)["isError"])

    def test_schema_failure_returns_none(self):
        """A page that fails the schema gives None."""
        call, _ = self._pager([_row("a.pdf")])
        self.assertIsNone(list_all(call, {}, lambda r: False))

    def test_page_count_is_bounded(self):
        """A wrong total cannot make the loop run without end."""
        call, calls = self._pager([_row(f"{i}.pdf") for i in range(600)], total=10**9)
        list_all(call, {}, lambda r: True)
        self.assertLessEqual(len(calls), 20)


if __name__ == "__main__":
    unittest.main()
