-- +goose Up
-- Change merge strategy default from 'rebase' to 'merge' (GitHub's default).
-- Update existing rows that still have the old default.
UPDATE repos SET merge_strategy = 'merge' WHERE merge_strategy = 'rebase';

-- +goose Down
UPDATE repos SET merge_strategy = 'rebase' WHERE merge_strategy = 'merge';
