-- Minimal, synthetic pre-release schema. Contains no production identities.
-- Keep this fixture unchanged when adding new fields to the current models.
CREATE TABLE users (id integer PRIMARY KEY AUTOINCREMENT, created_at datetime, updated_at datetime, deleted_at datetime, telegram_id integer NOT NULL, username text NOT NULL, abs_user_id text NOT NULL, security_code text NOT NULL, points integer DEFAULT 0, status text DEFAULT 'active');
INSERT INTO users(telegram_id,username,abs_user_id,security_code,points,status) VALUES(42,'legacy_player','legacy-abs','legacy-hash',321,'active');
CREATE TABLE inventories (id integer PRIMARY KEY AUTOINCREMENT, created_at datetime, updated_at datetime, deleted_at datetime, user_id integer NOT NULL, item_name text NOT NULL, quantity integer DEFAULT 0);
INSERT INTO inventories(user_id,item_name,quantity) VALUES(42,'回气丹',7);
CREATE TABLE system_configs (id integer PRIMARY KEY AUTOINCREMENT, created_at datetime, updated_at datetime, deleted_at datetime, key text NOT NULL, value text);
INSERT INTO system_configs(key,value) VALUES('legacy_custom','preserve-me');
CREATE TABLE treasure_rounds (id integer PRIMARY KEY AUTOINCREMENT, created_at datetime, chat_id integer NOT NULL, day_key text NOT NULL, status text NOT NULL, rewards_json text NOT NULL, expires_at datetime NOT NULL);
INSERT INTO treasure_rounds(chat_id,day_key,status,rewards_json,expires_at) VALUES(-123,'2026-10-28','active','[0,0,0,0,2,2,2,5,10]','2050-01-01 00:00:00+08:00');
