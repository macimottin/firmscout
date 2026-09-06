-- +goose Up
-- +goose StatementBegin

-- Phase 2 follow-up: make "one open review item per subject" a property of the schema.
--
-- 00002 added review_items_subject_idx, a plain index, to make "is there already an item
-- for this candidate" a fast question. Nothing ever asked it, and nothing stopped the
-- answer being "three". The job queue delivers at least once, and ValidateCandidate
-- filed its item with a plain INSERT and a freshly minted id, so a redelivered validation
-- -- or one whose later steps failed and was retried -- filed another copy of the same
-- decision. Every copy pointed at the same candidate, and a queue whose usefulness
-- depends on staying short grew one row per delivery.
--
-- The application now looks for the existing item first (ReviewRepository.FindOpenBySubject)
-- and rewrites it rather than duplicating it. This index is what makes that a guarantee
-- instead of a convention: two workers racing on the same candidate cannot both win, and
-- the loser's INSERT surfaces as domain.ErrConflict rather than as a second queue row.
--
-- It is partial on the open states for two reasons. Resolved and dismissed items are
-- history, and a subject legitimately accumulates several of them over its life. And the
-- states named here are exactly the ones review_items_queue_idx and
-- ReviewRepository.List treat as "the queue", so what is unique is precisely what a human
-- can see.
--
-- The subject_type column is part of the key rather than assumed: a review item's subject
-- is a candidate release when there is one a reviewer can accept, and the conflict itself
-- when the disagreement has outlived every publishable candidate. Those are different
-- decisions about different rows and must not collide.
CREATE UNIQUE INDEX review_items_open_subject_idx
    ON review_items (subject_type, subject_id)
    WHERE state IN ('open', 'in_progress');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS review_items_open_subject_idx;
-- +goose StatementEnd
