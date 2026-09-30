# ทีมและสิทธิ์: สมาชิก workspace และการจำกัดสิทธิ์รายโปรเจค

เพิ่ม 2026-09-20 · migration `00019_project_access.sql` และ `00022_access_fixes.sql` (ผลการตรวจความปลอดภัยรอบแรก)

จนถึงตอนนี้ workspace หนึ่งมีได้แค่ owner ที่ถูกสร้างตอน bootstrap และไม่มีทางเพิ่มคน (`docs/backend-foundation.md` ระบุไว้ว่า "ยังไม่มี invitations") เอกสารนี้อธิบายสองเรื่องที่เกี่ยวกัน: การเพิ่ม/แก้/ถอดสมาชิก และการจำกัดให้สมาชิกบางคนเห็นเฉพาะโปรเจคที่ได้รับมอบหมาย โดยบังคับที่ **ฐานข้อมูล** ไม่ใช่ที่โค้ด Go

## 1. โมเดล

- **Identity** (`identity.users`) เป็นของคน ไม่ผูกองค์กร คนคนเดียวอยู่ได้หลาย workspace ด้วยอีเมลเดียวและรหัสผ่านเดียว
- **Membership** (`core.memberships`) คือ "คนนี้อยู่ workspace นี้ ด้วยสิทธิ์ระดับนี้" หนึ่งคนมีได้หนึ่ง role ต่อ workspace
- **Role** มีสี่ระดับตามที่ schema กำหนดไว้แล้ว: `owner` (ทุกอย่างรวมถึงสมาชิก), `admin` (จัดการอุปกรณ์และสมาชิก), `operator` (รับทราบ/ปิดการแจ้งเตือน), `viewer` (ดูอย่างเดียว)
- **Project access** (`core.member_projects`) คือรายการโปรเจคที่สมาชิกคนหนึ่งเห็น
  - owner/admin เห็นทุกอย่างเสมอ ตารางนี้ไม่มีผลกับเขา
  - operator/viewer ที่ **ไม่มีแถว** เห็นทุกอย่าง (พฤติกรรมเดิมก่อน migration นี้ การอัปเกรดจึงไม่เปลี่ยนสิ่งที่ใครเห็น)
  - operator/viewer ที่ **มีแถว** เห็นเฉพาะ gateway ของโปรเจคเหล่านั้นและทุกอย่างที่ห้อยอยู่ใต้ gateway นั้น · gateway ที่ยังไม่จัดโปรเจคจะถูกซ่อนจากเขา
- ไม่มีการส่งอีเมลใน on-premise จึง **ไม่มีคำเชิญ** owner สร้างบัญชีพร้อมรหัสผ่านแรกแล้วส่งให้เจ้าตัวเอง `core.memberships.must_change_password` บอก UI ให้บังคับเปลี่ยนรหัสผ่านก่อนใช้งานต่อ และมีเฉพาะ `POST /api/v1/auth/password` เท่านั้นที่ล้าง flag นี้ได้

## 2. การออกแบบ RLS

### 2.1 scope ถูกคำนวณโดยฐานข้อมูล ครั้งเดียวต่อ transaction

`Repository.tx` (`backend/internal/adapters/postgres/repository.go`) ตั้ง `app.user_id`/`app.tenant_id` ด้วย `set_config(..., true)` อยู่แล้ว migration นี้เพิ่มอีกหนึ่งคำสั่งต่อท้าย:

```sql
SELECT set_config('app.project_scope', coalesce(core.compute_project_scope(), ''), true);
```

`core.compute_project_scope()` — `STABLE SECURITY DEFINER SET search_path = pg_catalog` ไม่รับ argument — คืนค่า

| กรณี | คืน |
|---|---|
| `app.user_id` ว่าง (งานระบบ: ingest, offline scanner, notification worker) | `'*'` |
| `app.tenant_id` ว่าง (ช่วง login/refresh ก่อนรู้ workspace) | `'*'` |
| ผู้เรียกเป็น owner/admin ของ tenant ปัจจุบัน | `'*'` |
| ผู้เรียกไม่มีแถวใน `core.member_projects` | `'*'` |
| นอกนั้น | uuid ของโปรเจค คั่นด้วย `,` |

เป็น `SECURITY DEFINER` โดยตั้งใจ (ต่างจาก helper ข้ออื่น) เพราะ scope คือ *ผู้ตัดสิน* ว่าผู้เรียกอ่านอะไรได้ จึงต้องอ่านจากแถว membership จริง ไม่ใช่จากสิ่งที่ policy ของผู้เรียกยอมให้เห็น ซึ่งจะเป็นการอ้างวนกัน ฟังก์ชันไม่รับ argument และอ่านเฉพาะ setting สองตัว จึงไม่มีอะไรให้ inject

`StartSession` และ `RotateRefresh` เป็นสองที่ที่เพิ่งรู้ tenant กลาง transaction ทั้งคู่คำนวณ scope ใหม่พร้อมกับ `app.tenant_id`

### 2.2 fail closed

```sql
CREATE FUNCTION core.scope_all() RETURNS boolean LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
 SELECT current_setting('app.project_scope', true) = '*'
$$;
```

ทุก policy เรียกใช้มันในรูป `(SELECT core.scope_all())` — sub-query ที่ไม่อ้างถึงแถว planner จึงยกขึ้นเป็น **InitPlan** ประเมินครั้งเดียวต่อ statement แทนที่จะเรียกฟังก์ชันทุกแถว (migration `00022`) รูปที่คาดไว้จาก `EXPLAIN SELECT count(*) FROM core.sensor_samples;` คือ

```
Aggregate
  InitPlan 1 (returns $0)
    ->  Result                      -- core.scope_all() ครั้งเดียว
  ->  Seq Scan on sensor_samples
        Filter: ((tenant_id = core.tenant_id()) AND ($0 OR core.gateway_in_scope(gateway_id)))
```

เมื่อ scope เป็น `'*'` (ingest, owner) `$0` เป็น true และครึ่งหลังของ OR ไม่ถูกแตะเลย

ถ้า setting ไม่ถูกตั้งเลย `current_setting(..., true)` คืน NULL ซึ่งไม่เท่ากับ `'*'` และไม่ตรงกับ uuid ใด ๆ · connection ที่ไม่ได้ผ่าน `Repository.tx` จึง **ไม่เห็นอะไรเลย** ไม่ใช่เห็นทุกอย่าง `tests/integration_test.go` ยืนยันข้อนี้: transaction ดิบที่ตั้งแค่ `app.user_id`/`app.tenant_id` นับ `core.devices` ได้ 0 แถว และได้ 1 แถวหลังรันคำสั่ง `set_config` ของ scope

### 2.3 helper functions

ทุกตัว `STABLE`, `SET search_path = pg_catalog`, **ไม่ใช่** SECURITY DEFINER (ยกเว้น `compute_project_scope` ข้างบน) — ตั้งใจให้ EXISTS ข้างในถูกกรองด้วย policy ของตารางที่มันอ่าน ทำให้ลูกโซ่ gateway → project สอดคล้องกันเสมอ ทุกตัวขึ้นต้นด้วย `CASE WHEN core.scope_all() THEN true` เพื่อให้สาขาถูกที่สุดถูกตรวจก่อน (ingest และ session ของ owner จึงไม่เคยรัน EXISTS เลย)

| ฟังก์ชัน | คืน true เมื่อ |
|---|---|
| `core.scope_all()` | scope เป็น `'*'` |
| `core.project_in_scope(uuid)` | scope_all หรือ uuid อยู่ในรายการ (NULL = ไม่อยู่) |
| `core.gateway_in_scope(uuid)` | scope_all หรือมีแถวใน `core.gateways` ที่มองเห็นได้ (ซึ่งถูกกรองด้วย project policy) |
| `core.device_in_scope(uuid)` | scope_all หรือมีแถวใน `core.devices` ที่มองเห็นได้ |
| `core.site_in_scope(uuid)` | scope_all หรือมีแถวใน `core.sites` ที่มองเห็นได้ |
| `core.floor_in_scope(uuid)` | scope_all หรือมีแถวใน `core.floors` ที่มองเห็นได้ |
| `core.automation_in_scope(uuid)` | scope_all หรือมีแถวใน `core.automations` ที่มองเห็นได้ |
| `core.alert_in_scope(uuid)` | scope_all หรือมีแถวใน `core.alerts` ที่มองเห็นได้ |
| `core.asset_in_scope(text, uuid)` | scope_all หรือ (`gateway` → gateway_in_scope · `device` → device_in_scope) |
| `core.identity_in_scope(text)` | scope_all หรือมี `core.sensor_streams` ที่มองเห็นได้เคยได้ยิน identity นี้ |

### 2.4 policy

ทุก policy ของ scope เป็น `AS RESTRICTIVE ... TO aether_app` ที่มีทั้ง `USING` และ `WITH CHECK`

- **RESTRICTIVE** แปลว่า AND กับ policy `tenant_scope` เดิม แถวต้องอยู่ทั้งใน tenant และใน scope · ไม่ได้แทนที่การแยก tenant แต่ซ้อนทับ
- **TO aether_app** เพราะ `aether_owner` (NOLOGIN, เจ้าของ migration) ถูก FORCE RLS ด้วย ถ้า policy ครอบคลุมเขา SECURITY DEFINER lookup ที่ต้องทำงานก่อนมี identity — `core.lookup_gateway`, `core.lookup_mqtt_gateway`, `core.mqtt_provisioning_accounts`, `core.active_tenant_ids` — จะ fail closed และ gateway จะ ingest ไม่ได้ · runtime มีบทบาทเดียวคือ `aether_app` และ `postgres.Open()` ตรวจตอน startup ว่า role ของ API ไม่ใช่สมาชิกของ `aether_owner`

### 2.5 การจัดการสมาชิกและปัญหา policy วนซ้ำ

`core.memberships` เดิมมีแค่ `own_membership_read` และ `own_membership_insert` · policy ที่ให้ admin เห็นทั้ง workspace ต้องอ่าน `core.memberships` เอง ซึ่งจะวนซ้ำบนตารางเดียวกัน จึงแยกออกเป็น

```sql
CREATE FUNCTION core.is_tenant_admin() RETURNS boolean
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM core.memberships m
   WHERE m.tenant_id = core.tenant_id() AND m.user_id = identity.user_id() AND m.role IN ('owner','admin'))
$$;
```

และ `CREATE POLICY memberships_owner_access ON core.memberships FOR ALL TO aether_owner USING(true) WITH CHECK(true)` (รูปแบบเดียวกับ migration `00007`) เพื่อให้ฟังก์ชันนี้มองเห็นแถวใต้ FORCE RLS · policy ของ runtime ทั้งสี่ (`tenant_admin_membership_read/insert/update/delete`) เป็น `TO aether_app` เท่านั้น การประเมิน policy ในฐานะ `aether_owner` จึงเข้าฟังก์ชันซ้ำไม่ได้ · `REVOKE ALL ... FROM PUBLIC` และ `GRANT EXECUTE ... TO aether_app`

ตั้งแต่ `00035` policy ของ `core.memberships` บอกกฎเดียวกับที่ `members.go` บังคับ SQL ที่ข้าม repository จึงทำได้ไม่เกิน API:

| policy | เงื่อนไข (ทุก `is_tenant_*` เป็น scalar sub-select = InitPlan) |
|---|---|
| `tenant_admin_membership_insert` | **ถูกลบ** · การเพิ่มสมาชิกไปผ่าน `identity.create_member_identity` (definer) เท่านั้น |
| `own_membership_insert` | แถวของตัวเองใน tenant ปัจจุบัน **และ tenant ยังไม่มีสมาชิกเลย** (`core.tenant_unclaimed()`) · เหลือไว้ให้ `CreateAccount` ใส่ owner คนแรกของ workspace ใหม่ · เดิม (`00001`) identity ใดก็ใส่ตัวเองเข้า tenant id ที่ตั้งใน context ได้ |
| `tenant_admin_membership_update` | USING และ WITH CHECK: admin ของ tenant ปัจจุบัน · **ไม่ใช่แถวของตัวเอง** · แถว/ค่า `role='owner'` ได้เฉพาะเมื่อผู้เรียกเป็น owner (`core.is_tenant_owner()`) · ตรงกับ `UpdateMember` (ห้ามแก้ตัวเอง, `mayTouch`) · การโอนความเป็นเจ้าของคือ owner ตั้งอีกคนเป็น owner ผ่าน `UpdateMember` ซึ่งยังทำได้ · กฎ "owner คนสุดท้าย" ยังอยู่ใน Go ใต้ `memberLock` |
| `tenant_admin_membership_delete` | เงื่อนไขเดียวกับ update ฝั่ง USING · ตรงกับ `RemoveMember` |

`core.is_tenant_owner()` เป็น definer รูปแบบเดียวกับ `is_tenant_admin` (`search_path = pg_catalog, pg_temp`)

สิทธิ์ที่ให้ runtime ถูกจำกัดระดับคอลัมน์: `GRANT UPDATE(role)` เท่านั้น · `must_change_password` เขียนได้จากฟังก์ชันรหัสผ่านเท่านั้น และ `user_id`/`tenant_id` เขียนทับไม่ได้เลย

`core.member_projects` มี `own_member_projects_read` (แถวของตัวเอง) และ `tenant_admin_member_projects` (`FOR ALL`, owner/admin ของ tenant ปัจจุบัน) และไม่มี policy scope ทับตัวเอง จึงไม่วนซ้ำ

### 2.6 `identity.users`

`identity.users` ไม่มีคอลัมน์ tenant (คนเดียวอยู่หลายองค์กร) ตั้งแต่ migration `00035` จึงเปิด RLS แบบ **ENABLE + FORCE** ด้วย policy ที่อิงตัวตนและการเป็นสมาชิก แทน tenant_scope ปกติ

| policy | สำหรับ | เงื่อนไข |
|---|---|---|
| `users_owner_access` | `aether_owner` (FOR ALL) | `true` — ให้ SECURITY DEFINER ทุกตัวทำงานได้ (FORCE RLS คลุมเจ้าของตารางด้วย) |
| `users_self_read` | `aether_app` SELECT | `id = identity.user_id()` — ทุกคนเห็นแถวของตัวเอง (`MemberSelf`, `/me`) |
| `users_tenant_admin_read` | `aether_app` SELECT | owner/admin ของ tenant ปัจจุบัน เห็นเฉพาะ identity ที่มี membership ใน tenant นี้ (`ListMembers`, read-back หลัง `AddMember`) · `is_tenant_admin()` อยู่ใน scalar sub-select จึงเป็น InitPlan ครั้งเดียวต่อ statement |
| `users_insert` | `aether_app` INSERT | `id = identity.user_id()` **เท่านั้น** — สมัคร/bootstrap ซึ่ง transaction รันเป็น id ใหม่อยู่แล้ว · admin สร้างสมาชิกผ่าน `identity.create_member_identity` เท่านั้น ไม่ใช่ INSERT ตรง |

ไม่มี policy UPDATE/DELETE และ runtime ไม่มีสิทธิ์ทั้งสองอย่าง · **สิทธิ์ SELECT เป็นรายคอลัมน์** `id, email, name, created_at` เท่านั้น — `aether_app` อ่าน `password_hash` ตรง ๆ ไม่ได้เลยแม้แถวของตัวเอง (42501)

`postgres.CheckRuntimeRole` (เรียกจาก `postgres.Open()` ตอน startup) ไม่ยอมเปิดถ้า role ของ API มีสิทธิ์พิเศษหรือเป็นสมาชิก `aether_owner` · ถ้าตารางใดใน schema `core` **หรือ** `identity` (รวม partitioned parent `relkind='p'`) ไม่ได้เปิด RLS + FORCE · หรือถ้า `has_column_privilege(current_user,'identity.users','password_hash','SELECT')` เป็นจริง · หรือถ้า runtime มีสิทธิ์ UPDATE (ทั้งตารางหรือคอลัมน์ใดก็ตาม) หรือ DELETE บน `identity.users`

login ตรวจรหัสผ่านใน Go แล้วค่อยเปิด session · `StartSession` อ่าน `identity.own_password_hash()` อีกครั้งใน transaction ของตัวเองและเทียบกับ hash ที่ `Service.Login` ตรวจผ่าน ถ้ารหัสผ่านถูกเปลี่ยนหรือ reset ในระหว่างนั้นจะได้ Unauthorized

ทางที่ RLS ให้ไม่ได้ ถูกยกให้ SECURITY DEFINER (ตัวใหม่และตัวที่แก้ใน `00035` ใช้ `search_path = pg_catalog, pg_temp`, `REVOKE ALL FROM PUBLIC`, `GRANT EXECUTE TO aether_app`)

| ฟังก์ชัน | ทำอะไร | ป้องกันอย่างไร |
|---|---|---|
| `identity.login_candidate(text)` | id/email/name/password_hash ของอีเมลนี้ สำหรับ login (`UserByEmail`) ซึ่งทำงานก่อนรู้ตัวตน | ค้นแบบตรงตัวหนึ่งแถว ไล่ทั้งตารางไม่ได้ · **แต่คืน hash** ดูข้อจำกัดด้านล่าง |
| `identity.any_user_exists()` | มีบัญชีใดอยู่แล้วหรือยัง (boolean) | ใช้บังคับ bootstrap ครั้งเดียวใน `CreateAccount` · RLS จะทำให้ `count(*)` เห็นแค่ตัวเองและนับได้ 0 เสมอ จึงต้องมาทางนี้ |
| `identity.own_password_hash()` | hash ของ `identity.user_id()` | ใช้ใน `ChangeOwnPassword` เพื่อตรวจรหัสเดิม (Argon2id ยังอยู่ใน Go) · ไม่มีตัวตน → NULL |
| `identity.create_member_identity(uuid, text, text, text, text)` | เพิ่มสมาชิกแบบ atomic: สร้าง identity ใหม่ หรือรับ identity ที่ไม่อยู่ workspace ใดเลยกลับมา (adopt) แล้ว insert membership · ผลคือ `created` / `adopted` / `self` / `taken` / `refused` | ผู้เรียกต้องเป็น owner/admin ของ tenant ปัจจุบัน และเฉพาะ owner เพิ่ม owner ได้ (ไม่งั้น `refused`) · **ล็อกแถว identity `FOR UPDATE` ก่อนนับ membership** และเป็น VOLATILE จึงนับด้วย snapshot ใหม่หลังรอล็อก: สอง workspace adopt คนเดียวกันพร้อมกัน คนที่สองได้ `taken` · อีเมลใหม่ที่สร้างพร้อมกันชนกันที่ unique index (`ON CONFLICT DO NOTHING` → `taken`) · adopt แล้ว **แทนชื่อและรหัสผ่านด้วยค่าที่ admin ใส่** ไม่มีอะไรของบัญชีเดิมโผล่ออกมา · `taken` ไม่คืน id และไม่เขียนอะไร |
| `core.identity_membership_count(uuid)` | จำนวน workspace ที่ identity นี้อยู่ (ทุก tenant) | ต้องเป็น admin **และ identity ต้องเป็นสมาชิกของ tenant ปัจจุบัน** ไม่งั้นคืน -1 (ตั้งแต่ `00035`) · ใช้ใน `ResetMemberPassword` เท่านั้น · ตัวเลขไม่เคยออกจาก server |
| `identity.tenant_last_seen()` | เวลา session ล่าสุดของสมาชิกแต่ละคน | ต้องเป็น admin · จำกัดที่ `core.tenant_id()` |
| `identity.purge_tenant_sessions(uuid)` | ลบ session + refresh token ของสมาชิกใน tenant นี้ | ต้องเป็น admin · ห้ามเป้าหมายเป็นตัวเอง · จำกัดที่ `core.tenant_id()` · **ถ้าเป้าหมายเป็น owner ผู้เรียกต้องเป็น owner** |
| `identity.set_member_password(uuid, text)` | ตั้งรหัสผ่านใหม่ + ตั้ง must_change_password | ต้องเป็น admin · ห้ามเป้าหมายเป็นตัวเอง · เป้าหมายต้องอยู่ tenant นี้ **และไม่มีที่อื่น** · **ถ้าเป้าหมายเป็น owner ผู้เรียกต้องเป็น owner** · ตั้งแต่ `00035` ล็อกแถว identity `FOR UPDATE` ก่อนนับ จึงไม่สลับกับการ adopt ที่เกิดพร้อมกัน |
| `identity.change_own_password(text)` | เปลี่ยนรหัสผ่านตัวเอง + ล้าง must_change_password | เขียนเฉพาะแถวของ `identity.user_id()` · ผู้เรียกตรวจรหัสผ่านเดิมแล้วใน transaction เดียวกัน |

กฎ "owner แตะได้เฉพาะ owner" ถูกย้ายลงมาอยู่ใน SQL ด้วย (migration `00022`) ไม่ใช่อยู่แค่ชั้น Go: ถ้าวันหนึ่ง handler ลืมตรวจ หรือมี path ใหม่เรียกฟังก์ชันตรง ๆ ฐานข้อมูลยังปฏิเสธเอง · integration test เรียกสองฟังก์ชันนี้ผ่าน connection ของ `aether_app` ตรง ๆ ในฐานะ admin เพื่อพิสูจน์ข้อนี้โดยไม่ผ่าน route

`00035` ยังเปลี่ยน policy สมาชิกของ `00019` (`tenant_admin_membership_*` และ `tenant_admin_member_projects`) ให้เรียก `(SELECT core.is_tenant_admin())` เป็น InitPlan ครั้งเดียวต่อ statement แทนต่อแถว

**สิ่งที่ RLS นี้ได้ และไม่ได้**

- ได้: bug ในชั้น query เช่นลืม `WHERE`, `SELECT *` หรือ join ผิดทิศ ไม่ทำให้ hash หรือรายชื่อคนของ workspace อื่นหลุดอีกแล้ว เพราะ runtime ไม่มีสิทธิ์คอลัมน์ hash และเห็นเฉพาะแถวที่ policy อนุญาต
- ไม่ได้: `identity.login_candidate` ยังคืน hash ให้ใครก็ได้ที่รัน SQL เป็น `aether_app` และรู้อีเมล (ส่วน `set_member_password` และ `own_password_hash` ทำงานตาม id) · ผู้ที่ได้ SQL ในนาม `aether_app` จึงยังดึง hash ของอีเมลที่รู้ได้ทีละตัว — **ไม่ได้ "แรงเท่า route login"** เพราะ route login ไม่เคยคืน hash · งานต่อที่วางไว้: role สำหรับ login โดยเฉพาะ (`aether_auth`) บน pool แยก ให้เป็น role เดียวที่ EXECUTE `login_candidate` ได้ (แตะ `infra/` จึงทำหลัง PITR)

**การไล่ถามอีเมล (enumeration) ผ่าน `POST /api/v1/members`** ยังเหลือสัญญาณอยู่หนึ่งอย่าง: อีเมลที่ยังไม่มีบัญชี (หรือไม่อยู่ workspace ใดแล้ว) ได้ **201** อีเมลที่อยู่ workspace อื่นได้ **409** · สิ่งที่ลดความเสี่ยง: route นี้ใช้ได้เฉพาะ owner/admin · ถูกจำกัดด้วย `domain.MaxMembers` ต่อ workspace (201 ทุกครั้งกินโควตา และต้องลบสมาชิกออกเองทีละคน) · 409 ไม่มีรายละเอียดและไม่คืน id · ทุกครั้งที่ถูกปฏิเสธจะบันทึกใน `core.audit_logs` เป็น `member.add_refused:<เหตุผล>` — `taken` (อยู่ workspace อื่นหรืออยู่ที่นี่แล้ว), `self`, `role` (ให้ role ที่ไม่มีสิทธิ์ให้) หรือ `limit` (ครบ `MaxMembers`) · `audit_logs` ไม่มีคอลัมน์รายละเอียด เหตุผลจึงอยู่ท้ายชื่อ action · target เป็น nil UUID ไม่ระบุ identity จึงเห็นร่องรอยการไล่ถาม · **ผู้เรียกที่ถูกปฏิเสธครบ 20 ครั้งใน 24 ชั่วโมง** (`postgres.AddRefusalLimit`, นับต่อ actor ต่อ workspace ด้วย partial index `audit_logs_add_refused`) จะได้ **429** ก่อนระบบตรวจอะไรต่อ และครั้งที่ได้ 429 ไม่ถูกนับเพิ่ม · adopt แล้วคืนและเก็บชื่อที่ admin ใส่ ไม่ใช่ชื่อเดิมของบัญชี

สิ่งที่ `backend/tests/identity_rls_test.go` พิสูจน์ผ่าน connection ของ `aether_app` ตรง ๆ:

- owner ของ tenant A เห็น identity ของ A พอดี (ค้นอีเมลของ B ได้ 0 แถว) · viewer เห็นแค่ตัวเอง · ไม่มี context เห็น 0 แถว
- `SELECT password_hash`, `UPDATE identity.users` และ INSERT identity ที่ไม่ใช่ตัวเอง (แม้เป็น owner) ได้ 42501
- non-admin เรียก `create_member_identity` ได้ `refused` · อีเมลของ workspace อื่นได้ `taken` ไม่มี id
- สอง transaction adopt คนเดียวกันพร้อมกัน คนที่สองรอล็อกแล้วได้ `taken` และชื่อที่เก็บเป็นชื่อที่ใส่ใหม่ (ลองเอา `FOR UPDATE` ออกแล้ว test นี้ fail)
- `set_member_password` ถือล็อกแถว identity (probe `FOR UPDATE NOWAIT` ได้ 55P03) · reset รหัสของสมาชิกที่อยู่ workspace เดียวได้ 204 และของ identity ที่แชร์ได้ 409 · `identity_membership_count` ของคนนอก workspace ได้ -1
- การปฏิเสธถูก audit เป็น `member.add_refused:taken|self|role` โดยไม่ระบุ identity · ครบ 20 ครั้งใน 24 ชั่วโมงได้ 429 เฉพาะ actor คนนั้น และพ้นหน้าต่าง 24 ชั่วโมงแล้วใช้ได้อีก
- `create_member_identity` เองได้ `refused` เมื่อ admin ขอ `owner`, role `root` หรืออาร์กิวเมนต์ใดเป็น NULL
- ในฐานะ `aether_app`: admin/owner INSERT membership ตรง ๆ ไม่ได้ · ใส่ตัวเองเข้า workspace อื่นที่มีสมาชิกแล้วไม่ได้ · admin ตั้ง `role='owner'` ไม่ได้ (42501) · admin แก้/ลบแถว owner และใครก็แก้แถวของตัวเองไม่ได้ (0 แถว) · ส่วนที่ Go อนุญาต (admin เปลี่ยน role คนที่ไม่ใช่ owner, owner ตั้ง owner ใหม่และลด owner อื่น, สมัครบัญชีใหม่) ยังทำได้
- `StartSession` ที่ได้ hash ไม่ตรงกับที่เก็บอยู่ (เปลี่ยนไปแล้วหรือว่าง) ได้ Unauthorized
- login, refresh, `/me`, `/members`, เปลี่ยนรหัสตัวเอง ยังทำงาน
- `CheckRuntimeRole` ปฏิเสธเมื่อ `identity.users` ไม่ FORCE RLS / ไม่เปิด RLS / runtime อ่าน `password_hash` ได้ / มี UPDATE (ตารางหรือคอลัมน์) หรือ DELETE · แต่ละกรณีจัดฉากใน transaction ของ admin ภายใต้ `SET LOCAL ROLE aether_app` แล้ว rollback ฐานข้อมูลทดสอบที่ใช้ร่วมกันจึงไม่ถูกแก้ค้างแม้ test ตายกลางทาง

Argon2id ยังอยู่ใน Go เท่านั้น `ChangeOwnPassword` ส่ง callback ตรวจ hash เข้าไปใน transaction จึงไม่มี hash หลุดออกจาก repository และไม่มีการแฮชใน SQL · `password_hash` ไม่เคยถูก serialize (`json:"-"` ใน `domain.User`)

ย้อนกลับ: `00035` มี Down ที่คืน grant SELECT ทั้งตาราง คืน body ของ `set_member_password`/`identity_membership_count` ตาม `00022` และ policy สมาชิกตาม `00001`/`00019` (สร้าง `tenant_admin_membership_insert` คืน) ลบ index `audit_logs_add_refused` ลบ policy/ฟังก์ชันใหม่ (รวม `is_tenant_owner`, `tenant_unclaimed`) และปิด RLS · ต้องย้อนโค้ด API ไปรุ่นก่อนหน้าด้วย เพราะโค้ดใหม่เรียกฟังก์ชันที่ Down ลบทิ้ง

## 3. ตารางทั้งหมดและวิธี scope

`core` ทุกตารางอยู่ภายใต้ ENABLE + FORCE RLS และมี policy `tenant_scope` อยู่แล้ว คอลัมน์ขวาคือ policy `project_scope` ที่ migration `00019` เพิ่ม

| ตาราง | ผูกกับอะไร | policy `project_scope` |
|---|---|---|
| `core.tenants` | workspace เอง | ไม่ scope (เป็นตัว workspace) |
| `core.memberships` | workspace | ไม่ scope · owner/admin policy แยกต่างหาก |
| `core.member_projects` | workspace | ไม่ scope · อ่านแถวตัวเองได้ + owner/admin |
| `core.projects` | `id` | `core.project_in_scope(id)` |
| `core.gateways` | `project_id` | `core.project_in_scope(project_id)` · project NULL ถูกซ่อนจากสมาชิกที่ถูกจำกัด |
| `core.devices` | `gateway_id` | `core.gateway_in_scope(gateway_id)` |
| `core.gateway_packets` | `gateway_id` | `core.gateway_in_scope(gateway_id)` |
| `core.sensor_streams` | `gateway_id` | `core.gateway_in_scope(gateway_id)` |
| `core.sensor_samples` | `gateway_id` | `core.gateway_in_scope(gateway_id)` |
| `core.stream_state` | `gateway_id` | `core.gateway_in_scope(gateway_id)` |
| `core.device_events` | `gateway_id` | `core.gateway_in_scope(gateway_id)` |
| `core.alerts` | `gateway_id` | `core.gateway_in_scope(gateway_id)` |
| `core.ble_history` | `gateway_id` | `core.gateway_in_scope(gateway_id)` |
| `core.mqtt_accounts` | `gateway_id` | `core.gateway_in_scope(gateway_id)` |
| `core.telemetry` | `device_id` | `core.device_in_scope(device_id)` (ผ่าน gateway ของอุปกรณ์) |
| `core.device_state` | `device_id` | `core.device_in_scope(device_id)` |
| `core.notifications` | `alert_id` | `core.alert_in_scope(alert_id)` |
| `core.sites` | `project_id` | `core.project_in_scope(project_id)` · project NULL ถูกซ่อน |
| `core.floors` | `site_id` | `core.site_in_scope(site_id)` |
| `core.floor_placements` | `floor_id` | `core.floor_in_scope(floor_id)` |
| `core.floor_images` | `floor_id` | `core.floor_in_scope(floor_id)` |
| `core.automations` | `project_id` | `core.project_in_scope(project_id)` · project NULL ถูกซ่อน |
| `core.automation_state` | `automation_id` | `core.automation_in_scope(automation_id)` |
| `core.automation_runs` | `automation_id` | `core.automation_in_scope(automation_id)` |
| `core.asset_records` | `asset_kind`,`asset_id` | `core.asset_in_scope(asset_kind, asset_id)` |
| `core.maintenance_plans` | `asset_kind`,`asset_id` | `core.asset_in_scope(asset_kind, asset_id)` |
| `core.maintenance_logs` | `asset_kind`,`asset_id` | `core.asset_in_scope(asset_kind, asset_id)` |
| `core.presence_state` | `external_id` (+ `gateway_id`, `candidate_gateway_id`) | `core.identity_in_scope(external_id)` **และ** gateway ทุกตัวที่แถวนั้นระบุต้องอยู่ใน scope — การรู้ว่ามี tag นี้ ไม่เท่ากับการรู้ว่ามันอยู่ในตึกของโปรเจคอื่น (`WITH CHECK` ใช้แค่ identity เพราะผู้เขียนคือ ingest ที่ scope เป็น `'*'`) |
| `core.audit_logs` | tenant | ไม่ scope · API ไม่มีเส้นทางอ่าน audit log เลย |
| `core.device_templates` | tenant | ไม่ scope · เป็นนิยาม decoder ไม่มีข้อมูลอุปกรณ์ · route เป็น owner/admin เท่านั้น |
| `core.studio_items` | tenant / community | ไม่ scope · เป็นนิยาม widget/decoder/dashboard · ค่าที่ dashboard วาดมาจากตารางที่ scope แล้ว · route เป็น owner/admin เท่านั้น |
| `core.notification_channels` | tenant | ไม่ scope · route เป็น owner/admin เท่านั้น ซึ่งเห็นทุกอย่างอยู่แล้ว |
| `core.alert_rules` | tenant | ไม่ scope · route เป็น owner/admin เท่านั้น |
| `core.automation_triggers` (migration `00020`) | `automation_id` | `core.automation_in_scope(automation_id)` |

`identity.sessions` และ `identity.refresh_tokens` ยังเป็น user-scoped RLS เหมือนเดิม migration นี้เพิ่มเฉพาะ policy ของ `aether_owner` เพื่อให้ฟังก์ชันใน §2.6 ทำงานได้

### ตารางที่ไม่มี gateway เป็นกุญแจ

`core.presence_state` (กุญแจคือ BLE identity) และ `core.asset_records` / `core.maintenance_plans` / `core.maintenance_logs` (กุญแจคือ asset id แบบ polymorphic) ถูก scope **ผ่าน join** ตามตารางข้างบน ไม่ใช่ถูกปล่อยผ่าน แต่ต้องเข้าใจว่าทั้งสองกลุ่มนี้เป็น **การป้องกันซ้อนชั้น** มากกว่าเป็นด่านเดียว เพราะเส้นทาง API ที่มีอยู่แคบอยู่แล้ว:

- `postgres/presence.go` · `Presence()` อ่าน `core.presence_state` ต่อเมื่อมี `core.devices` ที่ roaming และมี external_id ตรงกัน ซึ่งตัวมันเองถูก scope ด้วย gateway อยู่แล้ว · รายการ sighting ที่ตอบกลับมาจาก `core.sensor_streams` join `core.gateways` ตรง ๆ จึงถูกตัดด้วย gateway policy — ข้อนี้มี integration test ยืนยัน (สมาชิกที่ถูกจำกัดเห็น sighting ของ gateway โปรเจค A เท่านั้น ส่วน owner เห็นทั้งสอง)
- `postgres/assets.go` · ทุก row ของทะเบียนถูกสร้างจาก `core.gateways`/`core.devices` (`assetListSQL`) และทุกการเขียนเรียก `assetExists()` ที่พิสูจน์ก่อนว่า asset เป็นของ tenant และยังมีชีวิต · `core.asset_records`/`maintenance_*` ถูก LEFT JOIN เข้ากับ base นั้น จึงเข้าถึง asset นอก scope ไม่ได้อยู่แล้ว — integration test ยืนยันว่าสมาชิกที่ถูกจำกัดเห็น 2 รายการ (gateway A + device A) และ `GET /assets/gateway/{B}` ตอบ 404

policy ที่เพิ่มเข้าไปทำให้ query **ใหม่** ที่ไปแตะตารางเหล่านี้ตรง ๆ (เช่นรายงาน MA ข้ามอุปกรณ์) ยังคงถูกตัดโดยอัตโนมัติ แม้ผู้เขียนจะลืมกรอง

### Index ที่ EXISTS ใช้

`core.gateways(id)`, `core.devices(id)`, `core.sites(tenant_id,id)`, `core.floors(tenant_id,id)`, `core.automations(tenant_id,id)`, `core.alerts(tenant_id,id)` เป็น primary key อยู่แล้ว · `core.sensor_streams(tenant_id,external_id,last_seen DESC)` มาจาก migration `00012` · `core.gateways(tenant_id,project_id)` มาจาก `00011` · migration นี้เพิ่ม `core.member_projects(tenant_id,project_id)` และ `core.memberships(user_id)` (ตัวหลังใช้โดย `core.member_tenant_count`)

เส้นทาง ingest รันด้วย scope `'*'` และ `scope_all()` เป็นสาขาแรกใน `CASE` ทุกฟังก์ชัน จึงไม่มี EXISTS ใด ๆ ถูกรันเลยระหว่าง ingest หรือใน session ของ owner ต้นทุนคือการเทียบ string หนึ่งครั้งต่อแถว บวกกับ `core.compute_project_scope()` หนึ่งครั้งต่อ transaction (สำหรับงานระบบคือ 0 query เพราะสาขาแรกเช็ค `identity.user_id() IS NULL`)

## 4. API

| Call | สิทธิ์ | ผลลัพธ์ |
|---|---|---|
| `GET /api/v1/me` | ทุกคนที่เข้าระบบ | `{user_id, tenant_id, email, name, role, project_ids \| null, must_change_password, deployment_mode}` · `project_ids` เป็น null เมื่อเห็นทุกโปรเจค |
| `GET /api/v1/members` | owner/admin | รายชื่อสมาชิก: user id, อีเมล, ชื่อ, role, `project_ids`, `must_change_password`, `created_at`, `last_seen_at` |
| `POST /api/v1/members` | owner/admin | `{email, name?, role, password, project_ids[]}` → ตอบแถวสมาชิกที่สร้าง · อีเมลใหม่ = สร้าง identity ใหม่ · อีเมลที่มี identity อยู่แล้วแต่ **ไม่ได้อยู่ workspace ไหนเลย** (เคยถูกถอดออก) = รับกลับเข้ามาพร้อมรหัสผ่านที่ส่งมา · อีเมลที่ identity ยังผูกกับ workspace ใดอยู่ = **409** ไม่ทำอะไรทั้งสิ้น |
| `POST /api/v1/members/{user_id}/update` | owner/admin | `{role, project_ids[]}` |
| `POST /api/v1/members/{user_id}/remove` | owner/admin | ลบ membership และลบ session + refresh token ของเขาใน workspace นี้ใน transaction เดียวกัน |
| `POST /api/v1/members/{user_id}/reset-password` | owner/admin | `{password}` · 409 ถ้า identity นั้นอยู่ workspace อื่นด้วย · ตัด session และตั้ง `must_change_password` |
| `POST /api/v1/auth/password` | ผู้ที่เข้าระบบ | `{current_password, new_password}` · ตรวจ Origin ตรงตัวเหมือน login · ตรวจรหัสผ่านเดิม · เพิกถอน session **อื่น** ทุกอันของ identity นี้ · ล้าง `must_change_password` · อยู่ใน budget 10 ครั้ง/นาที เดียวกับ login |

### ทำไมถึงห้ามผูก identity ของ workspace อื่น

login เลือก workspace ให้อัตโนมัติเมื่อ identity มี membership เพียงอันเดียว การแอบเพิ่ม membership ที่สองให้บัญชีของคนแปลกหน้าจึงเปลี่ยนว่า "ครั้งหน้าเขา login ไปโผล่ที่ไหน" — owner ที่ไม่หวังดีล็อกคนอื่นออกจากองค์กรของตัวเองได้โดยไม่ต้องรู้รหัสผ่านเลย และคำตอบที่แยกแยะได้ (`existing_identity`) ก็กลายเป็นเครื่องมือถามทั้งแพลตฟอร์มว่า "อีเมลนี้มีบัญชีไหม" · ตอนนี้ทั้งสองกรณีตอบ 409 เหมือนกับการเพิ่มคนซ้ำ ไม่มี `detail` ไม่มีอะไรเพิ่ม · สัญญาณที่ยังเหลือคือ 201 กับ 409 (อีเมลว่างหรือถูกใช้อยู่) ซึ่งจำกัดไว้ที่ owner/admin, `MaxMembers`, ถูก audit เป็น `member.add_refused:*` และได้ 429 เมื่อถูกปฏิเสธครบ 20 ครั้งใน 24 ชั่วโมง (§2.6) · ถ้าคนๆ นั้นควรอยู่สองที่จริง ต้องใช้คนละอีเมล จนกว่าจะมีระบบคำเชิญที่เจ้าของบัญชีกดยอมรับเอง

### must_change_password ถูกบังคับ ไม่ใช่แค่บอก

`must_change_password` ถูกส่งกลับใน response ของ login และ refresh และ `Authorize` อ่านค่ามันใหม่ทุก request · ตราบใดที่ยังเป็น true ทุก route ตอบ **403** พร้อม `detail: "password_change_required"` ยกเว้นสามทางที่จำเป็นต่อการแก้: `POST /api/v1/auth/password`, `POST /api/v1/auth/logout` และ `GET /api/v1/me` · WebSocket ก็ปฏิเสธ session แบบนี้เช่นกัน และตัดสายทิ้งถ้า flag กลับมาเป็น true ระหว่างทาง (ตรวจซ้ำทุก 60 วินาที) · แปลว่ารหัสผ่านที่ส่งกันปากต่อปากไม่เคยกลายเป็นรหัสผ่านถาวร

### กฎที่บังคับในฝั่ง server

ทุกข้อถูกตัดสินใน transaction เดียวกับที่เขียน ภายใต้ advisory lock ต่อ workspace (`postgres/members.go`) ไม่ใช่แค่ใน handler:

- ไม่มีใครแก้หรือถอด membership ของตัวเองผ่าน route เหล่านี้ (ป้องกันการล็อกตัวเองออกและการเลื่อนขั้นตัวเอง)
- admin สร้าง/แก้/ถอด `owner` ไม่ได้ และมอบ role `owner` ไม่ได้
- owner คนสุดท้ายถอดหรือลดขั้นไม่ได้ (409)
- สูงสุด 200 คนต่อ workspace (409)
- อีเมลถูกแปลงเป็นตัวพิมพ์เล็กและตรวจแบบอนุรักษ์นิยม (ที่อยู่เดียว ไม่มีชื่อแสดง มีจุดในโดเมน ≤254 byte)
- รหัสผ่านใช้เส้นทาง Argon2id เดิมและกฎ 12–128 byte เดิม และถูกแฮชเสมอแม้จะถูกปฏิเสธทีหลัง เพื่อให้เวลาที่ใช้เท่ากันทุกกรณี
- response ไม่เคยมี password hash และการปฏิเสธไม่บอกอะไรเกี่ยวกับ identity ที่อยู่ workspace อื่น — 409 เปล่า ๆ เหมือนการเพิ่มคนซ้ำ
- ทุกการเปลี่ยนแปลงถูกบันทึกลง `core.audit_logs`: `member.added`, `member.updated`, `member.removed`, `member.password_reset`, `account.password_changed`

## 5. หน้าเว็บ

`frontend/app/team/team.tsx` (`TeamPage`) · toolbar เดียวสูง 42 px (หัวข้อ, จำนวนคน, ค้นหา, "เพิ่มสมาชิก") แล้วตามด้วยตารางแน่น ๆ: อีเมล, สิทธิ์ (เป็น select สำหรับ owner/admin), โปรเจคที่เห็นเป็นชิปสี (สีมาจาก `PROJECT_COLORS`), วันที่เพิ่ม, เข้าระบบล่าสุด และปุ่มจัดการ · รหัสผ่านแรกถูกสุ่มในเบราว์เซอร์ด้วย `crypto.getRandomValues` 20 ตัวอักษร แสดงครั้งเดียวพร้อมปุ่มคัดลอกและคำเตือนว่าต้องส่งให้เจ้าตัวเอง · ผู้ที่ไม่ใช่ owner/admin เห็นคำอธิบายแทนตาราง · ส่วน "บัญชีของฉัน" ใช้เปลี่ยนรหัสผ่านของตัวเอง · `fetchMe(client)` ถูก export ไว้ให้ shell เรียกใช้ภายหลัง

รหัสผ่านที่สุ่มมาคือความลับที่นั่งอยู่ในหน่วยความจำของเบราว์เซอร์ จึงถูกลบทันทีเมื่อปิดหน้าต่าง เมื่อออกจากหน้านี้ และอย่างช้าที่สุดภายในสองนาที (`SECRET_TTL_MS`) เพื่อไม่ให้เครื่องที่เปิดค้างไว้ค้างรหัสผ่านของคนอื่นไว้บนจอ

## 6. ข้อจำกัดที่ยอมรับ

1. **WebSocket signal เป็นระดับ workspace** `GET /ws` ส่งเฉพาะ "มีของชนิดนี้เปลี่ยน" ไม่มีข้อมูลติดไปด้วย ตอนเปิด socket ระบบอ่าน scope ของผู้ใช้หนึ่งครั้ง (`MemberSelf`) แล้ว `realtime.Hub` จะ **ตัด `gateway_id` ออก** จากทุก signal ที่ส่งให้สมาชิกที่ถูกจำกัด เขาจึงไม่รู้ว่า gateway ตัวไหนขยับ · สิ่งที่ยังเหลืออยู่และยอมรับไว้คือ **จังหวะเวลา**: เขารู้ว่า "มีบางอย่างชนิด packet/event/alert/inventory เกิดขึ้นใน workspace นี้เดี๋ยวนี้" แม้ต้นเหตุจะอยู่ในโปรเจคที่เขาไม่เห็น เมื่อ refetch ผ่าน REST ก็ยังไม่ได้ข้อมูลนั้น · การปิดช่องนี้ให้สนิทต้องให้ hub รู้ว่า gateway แต่ละตัวอยู่โปรเจคไหน ซึ่งแปลว่าต้อง query ฐานข้อมูลในเส้นทาง fan-out หรือใส่ project id ลงใน `pg_notify` payload — ทั้งสองทางแพงกว่าประโยชน์ในรอบนี้
2. **ไม่มีการส่งอีเมล จึงไม่มีคำเชิญและไม่มี reset ด้วยตัวเอง** owner ต้องส่งรหัสผ่านแรกให้เจ้าตัวเอง ถ้าสมาชิกลืมรหัสผ่าน owner ต้องตั้งให้ใหม่และส่งให้ใหม่
3. **หนึ่งคนหนึ่ง role ต่อ workspace** ไม่มี role รายโปรเจค `core.member_projects` จำกัดว่า *เห็นอะไร* ไม่ได้เปลี่ยนว่า *ทำอะไรได้* เช่น operator ที่ถูกจำกัดไว้โปรเจคเดียว ยังคงปิดการแจ้งเตือนได้ทุกรายการ **ที่เขาเห็น**
4. **`/api/v1/studio/sources` ยังเป็น owner/admin เท่านั้น** (กฎเดิมก่อน migration นี้) สมาชิกที่ถูกจำกัดจึงได้ 403 ไม่ใช่ผลลัพธ์ที่ถูกตัดให้แคบลง · ข้อมูลเบื้องหลัง (`ListPackets`, `StreamHistory`) ถูก scope แล้วและมี test ยืนยัน · `/api/v1/live` เปิดให้สมาชิกทุกระดับแล้ว และถูกตัดด้วย RLS ตามปกติ
5. **`login_candidate` ยังคืน hash ให้ SQL ในนาม `aether_app`** `identity.users` เปิด RLS แล้วตั้งแต่ `00035` (§2.6) แต่ login ต้องได้ hash ก่อนรู้ตัวตน ใครที่รัน SQL เป็น `aether_app` ได้จึงยังดึง hash ของอีเมลที่รู้ได้ทีละตัว · งานต่อ: role `aether_auth` บน pool แยกสำหรับ login · และ `POST /api/v1/members` ยังบอกได้ว่าอีเมลอยู่ workspace อื่น (409) หรือไม่ (201) โดยจำกัดไว้ที่ owner/admin, `MaxMembers`, audit `member.add_refused:*` และ 429 ที่ 20 ครั้งต่อ 24 ชั่วโมง
6. **การเปลี่ยนรหัสผ่านเพิกถอน session ในทุก workspace** เพราะรหัสผ่านเป็นของ identity ไม่ใช่ของ workspace · ส่วน `must_change_password` เป็นของ membership จึงถูกล้างเฉพาะ workspace ที่เปลี่ยน
7. **ตารางใหม่ต้องเพิ่ม policy เอง** `postgres.Open()` ตรวจตอน startup ว่าทุกตารางใน `core` และ `identity` เปิด FORCE RLS แต่ไม่ได้ตรวจว่ามี `project_scope` ครบ · migration ที่สร้างตาราง tenant ใหม่ต้องเพิ่มแถวในตาราง §3 และเพิ่ม policy ด้วย

## 7. คู่มือผู้ดูแล: เพิ่มช่างที่ให้เห็นไซต์เดียว

ตัวอย่าง: จ้างช่างมาดูแลเฉพาะ "โรงพยาบาล A" และไม่อยากให้เห็นลูกค้ารายอื่นใน workspace เดียวกัน

1. **จัด gateway ให้อยู่ในโปรเจคก่อน** เปิดหน้า "เชื่อมต่ออุปกรณ์" แล้วตรวจว่า gateway ของโรงพยาบาล A อยู่ในโปรเจค "โรงพยาบาล A" ทุกตัว
   ข้อนี้สำคัญ: **gateway ที่ยังไม่จัดโปรเจคจะถูกซ่อนจากช่างคนนี้** อุปกรณ์ทุกตัวที่ห้อยอยู่ใต้ gateway จะตามไปเอง ไม่ต้องตั้งทีละตัว
2. **ถ้ามีผังพื้นหรือ automation ของไซต์นั้น** ให้ตั้ง "โปรเจค" ของ site และของ automation เป็นโรงพยาบาล A ด้วย มิฉะนั้นช่างจะไม่เห็น (site และ automation ที่ไม่มีโปรเจคถูกซ่อนจากสมาชิกที่ถูกจำกัด)
3. เปิดเมนู **"ทีมและสิทธิ์"** กด **"เพิ่มสมาชิก"**
4. ใส่อีเมลของช่าง เลือกสิทธิ์
   - **ผู้ดูข้อมูล (viewer)** ถ้าให้ดูอย่างเดียว
   - **ผู้ปฏิบัติงาน (operator)** ถ้าให้กด "รับทราบ"/"ปิด" การแจ้งเตือนได้ด้วย
   - อย่าเลือก admin หรือ owner ถ้าต้องการจำกัดไซต์ เพราะสองระดับนี้เห็นทุกโปรเจคเสมอ
5. ในช่อง **"โปรเจคที่เห็น"** ติ๊กเฉพาะ "โรงพยาบาล A" (ถ้าไม่ติ๊กอะไรเลย = เห็นทุกโปรเจค)
6. ระบบจะแสดง **รหัสผ่านแรก 20 ตัวอักษร ครั้งเดียว** กดคัดลอกแล้วส่งให้ช่างด้วยตัวเอง — ระบบนี้ไม่ส่งอีเมล และหน้าจอจะลบรหัสผ่านทิ้งเองใน 2 นาที · ครั้งแรกที่เขาเข้าระบบ เขาจะ**ใช้งานอะไรไม่ได้เลย** จนกว่าจะตั้งรหัสผ่านของตัวเอง หน้าจออื่นจะขึ้นว่า password_change_required
7. **ตรวจว่าใช้ได้จริง** ให้ช่างเข้าระบบแล้วดูว่าหน้า "เชื่อมต่ออุปกรณ์" มีเฉพาะ gateway ของโรงพยาบาล A การแจ้งเตือนของไซต์อื่นจะไม่ขึ้นเลย
8. **เมื่อจบงาน** กลับมาที่ "ทีมและสิทธิ์" แล้วกดปุ่มถังขยะเพื่อนำออก เขาจะหลุดจากระบบทันที ไม่ต้องรอ token หมดอายุ ข้อมูลอุปกรณ์และประวัติทั้งหมดยังอยู่ครบ

**ถ้าระบบปฏิเสธด้วย "เพิ่มอีเมลนี้ไม่ได้"** แปลว่าอีเมลนั้นมีบัญชี Aether ที่ผูกกับ workspace อื่นอยู่แล้ว (หรือเป็นสมาชิกที่นี่อยู่แล้ว) ระบบจะไม่บอกว่ากรณีไหน และบันทึกการปฏิเสธทุกครั้งไว้ใน audit log เพื่อลดการใช้หน้านี้ไล่ถามว่าใครมีบัญชีบ้าง · ให้ขออีเมลอื่นของเขา หรือถ้าเขาเคยอยู่ใน workspace นี้แล้วถูกถอดออกไป การเพิ่มกลับเข้ามาจะทำงานตามปกติ และเขาจะได้รหัสผ่านแรกอันใหม่

## Module permissions and scoped administrators (2026-09-22)

Migration `00023_member_access.sql` supersedes the owner/admin scope rule above: only owners always have global project access. Admins with selected projects obey those projects and cannot manage members. Members without selected projects retain access to all projects.

`core.member_access` stores per-module restrictions: `none`, `read`, `write`. The owner or an unrestricted admin manages them via `/members/{user_id}/access`. Changes apply to the next API request in an existing session; navigation refreshes every 30 seconds. Write never elevates the base role: viewers remain read-only and operators retain their existing operational actions.

Feature APIs for floor plans, automation, assets, alerts and studio enforce module restrictions. Shared telemetry/inventory APIs remain readable within the user's project scope because several pages use them; hiding Overview is not a blanket denial of telemetry access.

Devices inherit the project of their registered gateway. Moving a gateway moves the scope of its devices. Project automations validate their referenced devices and gateways on save, enable and execution, and read conditions only within their project.
