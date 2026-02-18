-- Migration: Change user_id from UUID to VARCHAR(256)
-- Reason: Apple IAP transactions use "apple_xxx" format user_id when CreateServiceAccount fails.
--         This causes "invalid input syntax for type uuid" errors.
--         Align with subscription-service which already uses VARCHAR(256).
-- Date: 2026-02-18
-- Rollback: ALTER COLUMN user_id TYPE UUID USING user_id::UUID (only safe if no non-UUID data exists)

-- resources table
ALTER TABLE fulfillment.resources
    ALTER COLUMN user_id TYPE VARCHAR(256) USING user_id::VARCHAR;

-- entitlements table
ALTER TABLE fulfillment.entitlements
    ALTER COLUMN user_id TYPE VARCHAR(256) USING user_id::VARCHAR;
