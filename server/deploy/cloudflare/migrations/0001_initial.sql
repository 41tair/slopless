-- Slopless D1 schema, version 1.
CREATE TABLE IF NOT EXISTS "accounts" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "platform" TEXT NOT NULL, "username" TEXT NOT NULL, "rewarded_confirmations" INTEGER NOT NULL DEFAULT (0), "awarded_credits" INTEGER NOT NULL DEFAULT (0), "created_at" DATETIME NOT NULL, "first_marker_id" INTEGER NULL, CONSTRAINT "accounts_users_discovered_accounts" FOREIGN KEY ("first_marker_id") REFERENCES "users" ("id") ON DELETE SET NULL);
CREATE UNIQUE INDEX IF NOT EXISTS "account_platform_username" ON "accounts" ("platform", "username");

CREATE TABLE IF NOT EXISTS "confirmations" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "branch_id" INTEGER NOT NULL, "created_at" DATETIME NOT NULL, "rewarded_at" DATETIME NULL, "account_id" INTEGER NOT NULL, "user_id" INTEGER NOT NULL, CONSTRAINT "confirmations_accounts_confirmations" FOREIGN KEY ("account_id") REFERENCES "accounts" ("id") ON DELETE NO ACTION, CONSTRAINT "confirmations_users_confirmations" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE NO ACTION);
CREATE UNIQUE INDEX IF NOT EXISTS "confirmation_account_id_user_id" ON "confirmations" ("account_id", "user_id");
CREATE INDEX IF NOT EXISTS "confirmation_account_id_branch_id_rewarded_at" ON "confirmations" ("account_id", "branch_id", "rewarded_at");

CREATE TABLE IF NOT EXISTS "credentials" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "kind" TEXT NOT NULL, "name" TEXT NOT NULL DEFAULT (''), "secret_hash" TEXT NOT NULL, "created_at" DATETIME NOT NULL, "expires_at" DATETIME NOT NULL, "user_id" INTEGER NOT NULL, CONSTRAINT "credentials_users_credentials" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE NO ACTION);
CREATE UNIQUE INDEX IF NOT EXISTS "credentials_secret_hash_key" ON "credentials" ("secret_hash");

CREATE TABLE IF NOT EXISTS "credit_entries" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "amount" INTEGER NOT NULL, "balance_after" INTEGER NOT NULL, "reason" TEXT NOT NULL, "created_at" DATETIME NOT NULL, "account_id" INTEGER NULL, "confirmation_id" INTEGER NULL, "invitation_id" INTEGER NULL, "user_id" INTEGER NOT NULL, CONSTRAINT "credit_entries_accounts_credit_entries" FOREIGN KEY ("account_id") REFERENCES "accounts" ("id") ON DELETE SET NULL, CONSTRAINT "credit_entries_confirmations_credit_entries" FOREIGN KEY ("confirmation_id") REFERENCES "confirmations" ("id") ON DELETE SET NULL, CONSTRAINT "credit_entries_invitations_credit_entry" FOREIGN KEY ("invitation_id") REFERENCES "invitations" ("id") ON DELETE SET NULL, CONSTRAINT "credit_entries_users_credit_entries" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE NO ACTION);
CREATE UNIQUE INDEX IF NOT EXISTS "credit_entries_invitation_id_key" ON "credit_entries" ("invitation_id");
CREATE INDEX IF NOT EXISTS "creditentry_user_id_id" ON "credit_entries" ("user_id", "id");
CREATE UNIQUE INDEX IF NOT EXISTS "creditentry_confirmation_id_user_id" ON "credit_entries" ("confirmation_id", "user_id");

CREATE TABLE IF NOT EXISTS "idempotencies" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "key" TEXT NOT NULL, "fingerprint" TEXT NOT NULL, "response" BLOB NOT NULL, "expires_at" DATETIME NOT NULL, "user_id" INTEGER NOT NULL, CONSTRAINT "idempotencies_users_idempotencies" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE NO ACTION);
CREATE UNIQUE INDEX IF NOT EXISTS "idempotency_user_id_key" ON "idempotencies" ("user_id", "key");
CREATE INDEX IF NOT EXISTS "idempotency_expires_at" ON "idempotencies" ("expires_at");

CREATE TABLE IF NOT EXISTS "invitations" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "code" TEXT NOT NULL, "purchase_key" TEXT NOT NULL DEFAULT (''), "cost" INTEGER NOT NULL DEFAULT (0), "created_at" DATETIME NOT NULL, "used_at" DATETIME NULL, "issuer_id" INTEGER NULL, CONSTRAINT "invitations_users_issued_invitations" FOREIGN KEY ("issuer_id") REFERENCES "users" ("id") ON DELETE SET NULL);
CREATE UNIQUE INDEX IF NOT EXISTS "invitations_code_key" ON "invitations" ("code");
CREATE UNIQUE INDEX IF NOT EXISTS "invitation_issuer_id_purchase_key" ON "invitations" ("issuer_id", "purchase_key");

CREATE TABLE IF NOT EXISTS "marks" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "platform" TEXT NOT NULL, "username" TEXT NOT NULL, "user_id" INTEGER NOT NULL, CONSTRAINT "marks_users_marks" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE NO ACTION);
CREATE UNIQUE INDEX IF NOT EXISTS "mark_user_id_platform_username" ON "marks" ("user_id", "platform", "username");
CREATE INDEX IF NOT EXISTS "mark_platform_username" ON "marks" ("platform", "username");

CREATE TABLE IF NOT EXISTS "rate_events" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "policy" TEXT NOT NULL, "subject" TEXT NOT NULL, "units" INTEGER NOT NULL, "expires_at" DATETIME NOT NULL);
CREATE INDEX IF NOT EXISTS "rateevent_policy_subject_expires_at" ON "rate_events" ("policy", "subject", "expires_at");
CREATE INDEX IF NOT EXISTS "rateevent_expires_at" ON "rate_events" ("expires_at");

CREATE TABLE IF NOT EXISTS "users" ("id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, "username" TEXT NOT NULL, "password_hash" TEXT NOT NULL, "credits" INTEGER NOT NULL DEFAULT (0), "created_at" DATETIME NOT NULL, "invitation_id" INTEGER NULL, "inviter_id" INTEGER NULL, CONSTRAINT "users_invitations_redeemer" FOREIGN KEY ("invitation_id") REFERENCES "invitations" ("id") ON DELETE SET NULL, CONSTRAINT "users_users_invitees" FOREIGN KEY ("inviter_id") REFERENCES "users" ("id") ON DELETE SET NULL);
CREATE UNIQUE INDEX IF NOT EXISTS "users_username_key" ON "users" ("username");
CREATE UNIQUE INDEX IF NOT EXISTS "users_invitation_id_key" ON "users" ("invitation_id");

CREATE TABLE IF NOT EXISTS "__slopless_meta" ("id" INTEGER NOT NULL PRIMARY KEY CHECK ("id" = 1), "version" INTEGER NOT NULL);
INSERT OR IGNORE INTO "__slopless_meta" ("id", "version") VALUES (1, 0);

CREATE TABLE IF NOT EXISTS "__slopless_commit_outcomes" ("txid" TEXT NOT NULL PRIMARY KEY, "version" INTEGER NOT NULL, "changes" INTEGER NOT NULL, "last_insert_id" INTEGER NOT NULL, "returning_column" TEXT NOT NULL, "created_at" INTEGER NOT NULL) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS "__slopless_commit_outcomes_created_at" ON "__slopless_commit_outcomes" ("created_at");

CREATE TABLE IF NOT EXISTS "__slopless_ids" ("table_name" TEXT NOT NULL PRIMARY KEY, "next_id" INTEGER NOT NULL CHECK ("next_id" BETWEEN 1 AND 9007199254740991)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS "__slopless_schema" ("id" INTEGER NOT NULL PRIMARY KEY CHECK ("id" = 1), "version" INTEGER NOT NULL);

-- Accounts present before credit attribution was introduced retain their identity,
-- but no historical first marker or reward is inferred from active marks.
INSERT OR IGNORE INTO "accounts" ("platform", "username", "rewarded_confirmations", "awarded_credits", "created_at", "first_marker_id")
SELECT DISTINCT "platform", "username", 0, 0, strftime('%Y-%m-%d %H:%M:%f+00:00', 'now'), NULL FROM "marks";

INSERT INTO "__slopless_schema" ("id", "version") VALUES (1, 1)
ON CONFLICT ("id") DO UPDATE SET "version" = MAX("version", excluded."version");
UPDATE "__slopless_meta" SET "version" = "version" + 1 WHERE "id" = 1;
