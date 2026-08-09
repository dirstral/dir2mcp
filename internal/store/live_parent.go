package store

// liveParentDocument is the SQL predicate that binds a chunk's liveness to its
// parent document row (issue #707).
//
// A chunk is live only when its parent document is live. The parent is live
// when it is not tombstoned (deleted = 0) and not in the error state
// (status != 'error'). A chunk with no document row at all stays live, because
// the join is a LEFT JOIN and a missing parent is not evidence of a failure.
//
// The predicate needs the documents table joined under the alias `d`.
//
// Why the invariant is enforced here, and not by a delete on failure: a
// document goes to `error` for transient reasons too. A provider timeout, a
// rate limit, or a locked file all set the same status. A delete would throw
// away work that a later successful scan can reuse. This predicate hides the
// content while the document is broken, and shows it again the moment the
// document indexes again. It also keeps one rule in one place, instead of a
// cleanup step that each failure path must remember to call.
//
// PR #456 (issue #425) first added this predicate to the embed queue, so a
// failed document could not have more chunks embedded. The retrieval paths kept
// serving the chunks that the document embedded on an earlier good run. The
// same predicate now applies to lexical search, to vector-hit liveness, and to
// the related-document seed, so all of them agree about which chunks are live.
const liveParentDocument = `(d.rel_path IS NULL OR (d.deleted = 0 AND d.status != 'error'))`
