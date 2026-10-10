package pgstore_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
)

// 마이그레이션 린트(D49). 롤링 배포 동안 옛 코드와 새 코드가 같은 스키마 위에서 돌고, 마이그레이션은 서빙 중인 DB 에 돈다.
// 그래서 새 파일은 expand–contract 규칙을 지킨다 — 옛 코드를 깨는 DDL(지우기·이름 바꾸기·NOT NULL·타입 바꾸기·TRUNCATE)과 큰 표를 오래
// 잠그는 DDL(CONCURRENTLY 없는 인덱스 만들기·지우기, NOT VALID 없는 제약, 칸과 함께 거는 CHECK·REFERENCES·UNIQUE, 인덱스를 새로 만드는
// UNIQUE·PRIMARY KEY 제약, 표를 다시 쓰는 기본값, 같은 파일의 VALIDATE, WHERE 없는 UPDATE·DELETE)을 막는다. 린트가 속을 볼 수 없는
// DO 블록은 예외 주석(`-- lint: allow-do <이유>`)이 있어야 한다.
// 규칙은 docs/deploy.md "스키마 바꾸기" 절과 같다. 0001~0020 은 규칙 전에 적용된 파일이라 예외다(고치지 않는다).

// legacyMigrations 는 규칙 전에 적용된 파일이다. 늘리지 않는다.
var legacyMigrations = map[string]bool{
	"0001_init.sql": true, "0002_reads.sql": true, "0003_message_time.sql": true, "0004_channel_features.sql": true,
	"0005_customer_seq.sql": true, "0006_product.sql": true, "0007_workspaces.sql": true, "0008_waiting.sql": true,
	"0009_history.sql": true, "0010_conversation_per_request.sql": true, "0011_past_conversations.sql": true,
	"0012_direct_upload.sql": true, "0013_roles.sql": true, "0014_attachment_lease.sql": true,
	"0015_attachment_lease_token.sql": true, "0016_widget_faq.sql": true, "0017_metrics.sql": true,
	"0018_sec_sessions.sql": true, "0019_customer_sessions_forever.sql": true, "0020_customer_close.sql": true,
}

var (
	reWS          = regexp.MustCompile(`\s+`)
	reCreateTable = regexp.MustCompile(`^CREATE (?:UNLOGGED |TEMP |TEMPORARY )?TABLE (?:IF NOT EXISTS )?([\w."]+)`)
	reAlterTable  = regexp.MustCompile(`^ALTER TABLE (?:IF EXISTS )?(?:ONLY )?([\w."]+)`)
	reCreateIndex = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX (CONCURRENTLY )?(?:IF NOT EXISTS )?(?:[\w."]+ )?ON (?:ONLY )?([\w."]+)`)
	reAlterDrop   = regexp.MustCompile(`\bDROP ("[^"]*"|\w+)`)
	reDropTable   = regexp.MustCompile(`^DROP TABLE\b`)
	reDropIndex   = regexp.MustCompile(`^DROP INDEX\b`)
	reTruncate    = regexp.MustCompile(`^TRUNCATE\b`)
	reDo          = regexp.MustCompile(`^DO\b`)
	reAllowDo     = regexp.MustCompile(`--[ \t]*lint:[ \t]*allow-do[ \t]+\S`)
	reAddColumn   = regexp.MustCompile(`\bADD (?:COLUMN )?(?:IF NOT EXISTS )?(?:"[^"]*"|\w+) `)
	reInlineCheck = regexp.MustCompile(`\b(CHECK|REFERENCES)\b`)
	reUniquePK    = regexp.MustCompile(`\b(UNIQUE|PRIMARY KEY)\b`)
	reUsingIndex  = regexp.MustCompile(`\bUSING INDEX\b`)
	// reRewriteDefault 는 있던 표에 칸을 더할 때 표 전체를 다시 쓰게 하는 꼴이다. PG11+ 은 상수와 휘발성이 아닌 기본값(now()·
	// CURRENT_TIMESTAMP 는 STABLE 이라 ALTER 때 한 번 계산된다)을 카탈로그에만 적고 행을 다시 쓰지 않는다. 휘발성 함수(행마다 값이 다르다),
	// serial·identity(시퀀스 값을 행마다 채운다), 저장되는 생성 칸은 ACCESS EXCLUSIVE 를 쥔 채 표를 다시 쓴다(감사 31 실측: relfilenode 가 바뀐다).
	reRewriteDefault = regexp.MustCompile(`\bDEFAULT\b.*\b(RANDOM|GEN_RANDOM_UUID|UUID_GENERATE_V\w*|CLOCK_TIMESTAMP|TIMEOFDAY|NEXTVAL)\s*\(|\b(SMALL|BIG)?SERIAL[248]?\b|\bGENERATED\b.*\bAS IDENTITY\b|\bGENERATED ALWAYS AS\b.*\bSTORED\b`)
	reSetNotNull     = regexp.MustCompile(`\bSET NOT NULL\b`)
	reAlterType      = regexp.MustCompile(`\bALTER (?:COLUMN )?[\w"]+ (?:SET DATA )?TYPE\b`)
	reRename         = regexp.MustCompile(`\bRENAME\b`)
	reAddCheckFK     = regexp.MustCompile(`\bADD (?:CONSTRAINT [\w"]+ )?(FOREIGN KEY|CHECK)\b`)
	reNotValid       = regexp.MustCompile(`\bNOT VALID\b`)
	reValidate       = regexp.MustCompile(`\bVALIDATE CONSTRAINT\b`)
	reWhere          = regexp.MustCompile(`\bWHERE\b`)
	reWrite          = regexp.MustCompile(`^(?:WITH\b.*\b)?(UPDATE|DELETE)\b`)
	reConcurrent     = regexp.MustCompile(`\bCONCURRENTLY\b`)
	reDollarTag      = regexp.MustCompile(`^\$[A-Za-z_]*\$`)
)

// ALTER TABLE … DROP 다음에 와도 되는 낱말이다. DROP CONSTRAINT 는 CHECK 를 넓힐 때(DROP 뒤 ADD … NOT VALID) 쓴다.
// 그 밖의 DROP 은 칸을 지운다(COLUMN 은 생략할 수 있다).
var allowedAlterDrop = map[string]bool{"CONSTRAINT": true, "DEFAULT": true, "NOT": true, "IDENTITY": true, "EXPRESSION": true}

// codeOnly 는 주석을 지우고 문자열 안을 비운 뒤 대문자로, 공백을 하나로 만든다. 주석·문자열 안의 낱말로 잡지 않게 한다.
func codeOnly(stmt string) string {
	var b strings.Builder
	for i := 0; i < len(stmt); i++ {
		ch := stmt[i]
		switch {
		case ch == '-' && i+1 < len(stmt) && stmt[i+1] == '-':
			for i < len(stmt) && stmt[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case ch == '/' && i+1 < len(stmt) && stmt[i+1] == '*':
			end := strings.Index(stmt[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
			b.WriteByte(' ')
		case ch == '\'':
			j := i + 1
			for j < len(stmt) {
				if stmt[j] == '\'' {
					if j+1 < len(stmt) && stmt[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			b.WriteString("''")
			i = j
		case ch == '$' && reDollarTag.MatchString(stmt[i:]):
			// 함수 본문(달러 인용)도 비운다. 본문 안의 DELETE·RENAME 은 마이그레이션이 하는 일이 아니다.
			tag := reDollarTag.FindString(stmt[i:])
			end := strings.Index(stmt[i+len(tag):], tag)
			if end < 0 {
				return b.String()
			}
			b.WriteString("$$")
			i += len(tag) + end + len(tag) - 1
		default:
			b.WriteByte(ch)
		}
	}
	return strings.TrimSpace(reWS.ReplaceAllString(strings.ToUpper(b.String()), " "))
}

func normTable(name string) string {
	return strings.ToLower(strings.Trim(strings.TrimPrefix(name, "PUBLIC."), `"`))
}

// lintMigration 은 새 마이그레이션 파일 하나의 위반을 문장으로 돌려준다.
func lintMigration(body string) []string {
	stmts, err := pgstore.SplitStatements(body)
	if err != nil {
		return []string{"문장으로 나눌 수 없다: " + err.Error()}
	}
	noTx := pgstore.HasNoTransactionDirective(body)
	var out []string
	created := map[string]bool{}
	notValid, validate := false, false
	for _, raw := range stmts {
		s := codeOnly(raw)
		if s == "" {
			continue
		}
		bad := func(rule string) { out = append(out, rule+": "+s) }
		if m := reCreateTable.FindStringSubmatch(s); m != nil {
			created[normTable(m[1])] = true
		}
		if reDropTable.MatchString(s) {
			bad("DROP TABLE 은 옛 코드가 다 내려간 뒤의 배포에서 한다")
		}
		if m := reAlterTable.FindStringSubmatch(s); m != nil {
			for _, d := range reAlterDrop.FindAllStringSubmatch(s, -1) {
				// 인용한 이름("body")도 잡는다. 따옴표가 붙은 채라 낱말 목록에 없어 칸 지우기로 본다.
				if !allowedAlterDrop[d[1]] {
					bad("DROP COLUMN 은 옛 코드가 다 내려간 뒤의 배포에서 한다")
					break
				}
			}
			if existing := !created[normTable(m[1])]; existing {
				addCol := reAddColumn.MatchString(s) && !reAddCheckFK.MatchString(s) && !strings.Contains(s, " ADD CONSTRAINT ")
				switch {
				case reAddCheckFK.MatchString(s) && !reNotValid.MatchString(s):
					bad("있던 표에 FOREIGN KEY·CHECK 를 더할 때는 NOT VALID 로 걸고 VALIDATE 는 다음 파일에서 한다")
				case addCol && reInlineCheck.MatchString(s):
					bad("있던 표에 칸을 더하며 CHECK·REFERENCES 를 함께 걸지 않는다 — 칸만 더하고 ADD CONSTRAINT … NOT VALID 로 건다")
				}
				if reUniquePK.MatchString(s) && !reUsingIndex.MatchString(s) {
					bad("있던 표의 UNIQUE·PRIMARY KEY 는 잠근 채 인덱스를 만든다. CREATE UNIQUE INDEX CONCURRENTLY 뒤 다음 파일에서 ADD CONSTRAINT … USING INDEX 로 건다")
				}
				if addCol && reRewriteDefault.MatchString(s) {
					bad("있던 표에 휘발성 기본값·serial·identity·저장 생성 칸을 더하면 표를 다시 쓴다. 기본값 없이 더하고 값은 마이그레이션 밖에서 채운다")
				}
			}
		}
		if reTruncate.MatchString(s) {
			bad("TRUNCATE 는 표를 비우고 ACCESS EXCLUSIVE 를 쥔다. 옛 코드가 읽는 행이 사라진다")
		}
		if reDropIndex.MatchString(s) && !reConcurrent.MatchString(s) {
			bad("인덱스는 DROP INDEX CONCURRENTLY 로 지운다(-- migrate: no-transaction 파일)")
		}
		if reDo.MatchString(s) && !reAllowDo.MatchString(raw) {
			bad("DO 블록은 린트가 속을 보지 못한다. 평문 DDL 로 쓰거나 블록 바로 위에 `-- lint: allow-do <이유>` 를 단다")
		}
		if reSetNotNull.MatchString(s) {
			bad("SET NOT NULL 은 표 전체를 잠그고 훑는다. CHECK (칸 IS NOT NULL) NOT VALID → 다음 파일에서 VALIDATE 로 한다")
		}
		if reAlterType.MatchString(s) {
			bad("ALTER COLUMN TYPE 은 표를 다시 쓰고 옛 코드를 깬다. 새 칸을 더하고 옮긴다")
		}
		if reRename.MatchString(s) {
			bad("RENAME 은 옛 코드를 깬다. 새 이름을 더하고 옛 것은 나중에 지운다")
		}
		if m := reCreateIndex.FindStringSubmatch(s); m != nil && m[1] == "" && !created[normTable(m[2])] {
			bad("있던 표의 인덱스는 CREATE INDEX CONCURRENTLY 로 만든다(-- migrate: no-transaction 파일)")
		}
		if reNotValid.MatchString(s) {
			notValid = true
		}
		if reValidate.MatchString(s) {
			validate = true
		}
		if reWrite.MatchString(s) && !reWhere.MatchString(s) {
			bad("WHERE 없는 UPDATE·DELETE 는 표 전체를 고친다. 백필은 마이그레이션 밖에서 범위로 나눠 한다")
		}
		concurrent := reConcurrent.MatchString(s)
		if concurrent && !noTx {
			bad("CONCURRENTLY 는 트랜잭션 안에서 안 된다. 파일 머리에 " + pgstore.NoTransactionDirective + " 를 단다")
		}
		if noTx && !concurrent {
			bad("트랜잭션 없는 파일은 CONCURRENTLY 문장만 둔다 — 중간에 실패하면 앞 문장이 되돌려지지 않는다")
		}
	}
	if notValid && validate {
		out = append(out, "NOT VALID 와 VALIDATE CONSTRAINT 를 한 파일에 두지 않는다 — 같은 트랜잭션이면 NOT VALID 로 건 잠금을 쥔 채 표를 훑는다")
	}
	return out
}

// 가짜 파일로 규칙 하나하나가 잡히는지, 허용하는 꼴은 통과하는지 본다.
func TestMigrationLint_새파일의금지DDL을잡고허용하는꼴은통과시킨다(t *testing.T) {
	bad := []struct{ name, body, want string }{
		{"칸 지우기", "ALTER TABLE messages DROP COLUMN body;", "DROP COLUMN"},
		{"COLUMN 을 뺀 칸 지우기", "ALTER TABLE messages DROP body;", "DROP COLUMN"},
		{"표 지우기", "DROP TABLE snippets;", "DROP TABLE"},
		{"NOT NULL", "ALTER TABLE conversations ALTER COLUMN closed_by SET NOT NULL;", "SET NOT NULL"},
		{"타입 바꾸기", "ALTER TABLE messages ALTER COLUMN seq TYPE INT;", "ALTER COLUMN TYPE"},
		{"SET DATA TYPE", "ALTER TABLE messages ALTER seq SET DATA TYPE INT;", "ALTER COLUMN TYPE"},
		{"칸 이름 바꾸기", "ALTER TABLE messages RENAME COLUMN body TO text;", "RENAME"},
		{"표 이름 바꾸기", "ALTER TABLE messages RENAME TO msgs;", "RENAME"},
		{"CONCURRENTLY 없는 인덱스", "CREATE INDEX messages_body_idx ON messages (body);", "CONCURRENTLY 로"},
		{"CONCURRENTLY 없는 유일 인덱스", "CREATE UNIQUE INDEX IF NOT EXISTS m_idx ON public.messages (id);", "CONCURRENTLY 로"},
		{"NOT VALID 없는 CHECK", "ALTER TABLE messages ADD CONSTRAINT m_len CHECK (length(body) < 10);", "NOT VALID"},
		{"NOT VALID 없는 외래 키", "ALTER TABLE messages ADD FOREIGN KEY (agent_id) REFERENCES agents (id);", "NOT VALID"},
		{"같은 파일의 NOT VALID 와 VALIDATE",
			"ALTER TABLE messages ADD CONSTRAINT m_len CHECK (length(body) < 10) NOT VALID;\nALTER TABLE messages VALIDATE CONSTRAINT m_len;", "한 파일에"},
		{"WHERE 없는 UPDATE", "UPDATE conversations SET closed_by = 'AGENT';", "WHERE 없는"},
		{"WHERE 없는 DELETE", "DELETE FROM sessions;", "WHERE 없는"},
		{"WITH 로 시작하는 WHERE 없는 UPDATE", "WITH x AS (SELECT 1) UPDATE conversations SET closed_by = 'AGENT';", "WHERE 없는"},
		{"표시 없는 CONCURRENTLY", "CREATE INDEX CONCURRENTLY m_idx ON messages (body);", "트랜잭션 안에서"},
		{"트랜잭션 없는 파일의 다른 문장", "-- migrate: no-transaction\nCREATE INDEX CONCURRENTLY m_idx ON messages (body);\nALTER TABLE messages ADD COLUMN x INT;", "CONCURRENTLY 문장만"},
		// 감사 31 에서 더한 꼴
		{"인용한 칸 이름 지우기", `ALTER TABLE messages DROP "body";`, "DROP COLUMN"},
		{"인용한 칸 이름 COLUMN 과 지우기", `ALTER TABLE messages DROP COLUMN IF EXISTS "Body";`, "DROP COLUMN"},
		{"칸과 함께 거는 CHECK", "ALTER TABLE messages ADD COLUMN score INT CHECK (score > 0);", "칸을 더하며"},
		{"칸과 함께 거는 REFERENCES", "ALTER TABLE messages ADD COLUMN owner UUID REFERENCES agents (id);", "칸을 더하며"},
		{"COLUMN 을 뺀 칸과 함께 거는 REFERENCES", "ALTER TABLE messages ADD owner UUID REFERENCES agents;", "칸을 더하며"},
		{"UNIQUE 제약", "ALTER TABLE messages ADD CONSTRAINT m_uniq UNIQUE (conversation_id, seq);", "USING INDEX"},
		{"PRIMARY KEY 제약", "ALTER TABLE zz_old ADD PRIMARY KEY (id);", "USING INDEX"},
		{"칸과 함께 거는 UNIQUE", "ALTER TABLE messages ADD COLUMN ext TEXT UNIQUE;", "USING INDEX"},
		{"random() 기본값", "ALTER TABLE messages ADD COLUMN r DOUBLE PRECISION DEFAULT random();", "다시 쓴다"},
		{"gen_random_uuid() 기본값", "ALTER TABLE messages ADD COLUMN u UUID NOT NULL DEFAULT gen_random_uuid();", "다시 쓴다"},
		{"clock_timestamp() 기본값", "ALTER TABLE messages ADD COLUMN c TIMESTAMPTZ DEFAULT clock_timestamp ();", "다시 쓴다"},
		{"bigserial 칸", "ALTER TABLE messages ADD COLUMN n BIGSERIAL;", "다시 쓴다"},
		{"identity 칸", "ALTER TABLE messages ADD COLUMN n BIGINT GENERATED BY DEFAULT AS IDENTITY;", "다시 쓴다"},
		{"저장 생성 칸", "ALTER TABLE messages ADD COLUMN n2 BIGINT GENERATED ALWAYS AS (seq * 2) STORED;", "다시 쓴다"},
		{"TRUNCATE", "TRUNCATE snippets;", "TRUNCATE"},
		{"TRUNCATE TABLE", "TRUNCATE TABLE ONLY snippets RESTART IDENTITY;", "TRUNCATE"},
		{"CONCURRENTLY 없는 인덱스 지우기", "DROP INDEX IF EXISTS messages_body_idx;", "DROP INDEX CONCURRENTLY"},
		{"DO 블록", "DO $$ BEGIN EXECUTE 'ALTER TABLE messages DROP COLUMN body'; END $$;", "DO 블록"},
		{"이유 없는 DO 예외 주석", "-- lint: allow-do\nDO $$ BEGIN PERFORM 1; END $$;", "DO 블록"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			got := lintMigration(tc.body)
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("%q 를 잡지 않았다(%q 가 있어야 한다): %v", tc.body, tc.want, got)
			}
		})
	}

	good := []struct{ name, body string }{
		{"칸 더하기", "ALTER TABLE conversations ADD COLUMN note2 TEXT;"},
		{"새 표와 그 인덱스·제약", "CREATE TABLE zz (id INT PRIMARY KEY, ws UUID REFERENCES workspaces (id), v INT CHECK (v > 0));\nCREATE INDEX zz_v_idx ON zz (v);\nALTER TABLE zz ADD CONSTRAINT zz_v CHECK (v < 10);"},
		{"CHECK 넓히기", "ALTER TABLE conversation_closures DROP CONSTRAINT c_check;\nALTER TABLE conversation_closures ADD CONSTRAINT c_check CHECK (handler IN ('A','B')) NOT VALID;"},
		{"VALIDATE 만 있는 다음 파일", "ALTER TABLE conversation_closures VALIDATE CONSTRAINT c_check;"},
		{"NOT NULL 풀기", "ALTER TABLE sessions ALTER COLUMN expires_at DROP NOT NULL;"},
		{"기본값 지우기", "ALTER TABLE sessions ALTER COLUMN expires_at DROP DEFAULT;"},
		{"WHERE 있는 UPDATE", "UPDATE sessions SET idle_for = NULL WHERE subject_kind = 'CUSTOMER';"},
		{"트랜잭션 없는 파일의 CONCURRENTLY", "-- 인덱스\n-- migrate: no-transaction\nCREATE INDEX CONCURRENTLY IF NOT EXISTS m_idx ON messages (body);\nDROP INDEX CONCURRENTLY IF EXISTS old_idx;"},
		{"주석과 문자열 안의 낱말", "-- DROP COLUMN body; RENAME\nINSERT INTO snippets (title) VALUES ('DROP TABLE x; SET NOT NULL');"},
		{"함수 본문의 달러 인용", "CREATE FUNCTION zz() RETURNS trigger AS $$ BEGIN DELETE FROM sessions; RETURN NULL; END $$ LANGUAGE plpgsql;"},
		// 감사 31: PG11+ 은 상수·비휘발 기본값을 카탈로그에만 적는다(행을 다시 쓰지 않는다). now() 는 STABLE 이다.
		{"상수 기본값", "ALTER TABLE messages ADD COLUMN pinned BOOLEAN NOT NULL DEFAULT false;"},
		{"now() 기본값", "ALTER TABLE messages ADD COLUMN seen_at TIMESTAMPTZ NOT NULL DEFAULT now();"},
		{"기본값 바꾸기는 휘발성이어도 된다", "ALTER TABLE messages ALTER COLUMN body SET DEFAULT md5(random()::text);"},
		{"새 표의 칸 제약·serial·UNIQUE", "CREATE TABLE zz2 (id BIGSERIAL PRIMARY KEY, u UUID DEFAULT gen_random_uuid() UNIQUE);\nALTER TABLE zz2 ADD COLUMN v INT CHECK (v > 0) REFERENCES zz2 (id);\nALTER TABLE zz2 ADD COLUMN w UUID DEFAULT gen_random_uuid();"},
		{"인덱스로 거는 UNIQUE", "ALTER TABLE messages ADD CONSTRAINT m_uniq UNIQUE USING INDEX m_uniq_idx;"},
		{"CONCURRENTLY 로 인덱스 지우기", "-- migrate: no-transaction\nDROP INDEX CONCURRENTLY IF EXISTS messages_body_idx;"},
		{"이유를 단 DO 예외", "-- lint: allow-do 역할이 없을 때만 만든다 — CREATE ROLE 에 IF NOT EXISTS 가 없다\nDO $$ BEGIN CREATE ROLE zz_ro; EXCEPTION WHEN duplicate_object THEN NULL; END $$;"},
		{"인용한 칸 이름의 기본값 지우기", `ALTER TABLE messages ALTER COLUMN "body" DROP DEFAULT;`},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			if got := lintMigration(tc.body); len(got) > 0 {
				t.Errorf("%q 를 막았다: %v", tc.body, got)
			}
		})
	}
}

// 리포의 새 마이그레이션은 위 규칙을 지킨다. 0001~0020 만 예외이고 예외 목록은 늘리지 않는다.
func TestMigrationLint_리포의새마이그레이션은expand_contract규칙을지킨다(t *testing.T) {
	names, err := pgstore.MigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
		if legacyMigrations[n] {
			continue
		}
		body, err := pgstore.MigrationBody(n)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range lintMigration(string(body)) {
			t.Errorf("%s: %s", n, v)
		}
	}
	if len(legacyMigrations) != 20 {
		t.Errorf("예외 목록이 %d 개다. 0001~0020 의 20 개에서 늘리지 않는다", len(legacyMigrations))
	}
	for n := range legacyMigrations {
		if !have[n] {
			t.Errorf("예외 목록의 %s 가 리포에 없다", n)
		}
	}
}

// 적용된 파일은 고치지 않는다(CLAUDE.md). 이미 적용된 DB 는 고친 내용을 다시 돌리지 않아 DB 마다 스키마가 갈린다.
// 파일마다 sha256 을 migrations.sum 에 두고 대조한다. 새 파일은 그 줄을 더해야 통과한다.
func TestMigrations_적용된파일은바뀌지않고새파일은해시를더해야한다(t *testing.T) {
	raw, err := os.ReadFile("migrations.sum")
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("migrations.sum 의 줄 꼴이 틀렸다: %q", line)
		}
		pinned[f[1]] = f[0]
	}
	names, err := pgstore.MigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		body, err := pgstore.MigrationBody(n)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		want, ok := pinned[n]
		switch {
		case !ok:
			t.Errorf("%s 의 해시가 migrations.sum 에 없다. 이 줄을 더한다:\n%s  %s", n, got, n)
		case want != got:
			t.Errorf("%s 가 바뀌었다(적용된 파일은 고치지 않는다 — 새 번호의 파일로 앞으로 고친다). 고정 %s, 지금 %s", n, want, got)
		}
		delete(pinned, n)
	}
	for n := range pinned {
		t.Errorf("migrations.sum 의 %s 가 리포에 없다 — 적용된 파일을 지우지 않는다", n)
	}
}

// 트랜잭션 없는 파일은 문장 하나씩 보낸다. 문자열·인용 이름·달러 인용·주석 안의 ; 는 나누지 않고, 주석만 남은 조각은 버린다.
func TestSplitStatements_따옴표와달러인용과주석안의세미콜론은나누지않는다(t *testing.T) {
	got, err := pgstore.SplitStatements(`-- 머리 주석; 나누지 않는다
CREATE INDEX CONCURRENTLY a ON t (x) WHERE y <> 'a;''b';
/* 블록; 주석 */ SELECT "we;ird" FROM t;
CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql;
SELECT $1;
-- 꼬리 주석만;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("문장 %d 개, 4 개여야 한다: %s", len(got), fmt.Sprintf("%q", got))
	}
	if !strings.HasSuffix(got[0], "'a;''b'") || !strings.Contains(got[2], "SELECT 1; $body$") || got[3] != "SELECT $1" {
		t.Errorf("나눈 결과가 틀렸다: %q", got)
	}
	if _, err := pgstore.SplitStatements("SELECT 'unterminated;"); err == nil {
		t.Error("닫히지 않은 따옴표를 받았다")
	}
}
