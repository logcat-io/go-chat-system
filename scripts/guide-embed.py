#!/usr/bin/env python3
"""가이드 각 단계 끝에 그 단계가 소유한 파일의 전문을 심는다.

코드와 문서가 어긋나지 않게 복사하지 않고 생성한다. 코드를 고쳤으면 다시 돌린다:
    python3 scripts/guide-embed.py
마커 사이만 갈아 끼우므로 손으로 쓴 본문은 건드리지 않는다.

검사만 할 때(CI·verify.sh):
    python3 scripts/guide-embed.py --check
파일을 쓰지 않는다. 생성 결과가 지금 파일과 다르면 다른 파일 목록을 찍고 1 로 끝난다.
"""
import fnmatch, os, re, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GUIDE = os.path.join(ROOT, "docs", "guide")
START, END = "<!-- 정답지:start -->", "<!-- 정답지:end -->"

# 단계 → 그 단계에서 완성되는 파일. 뒤 단계가 손대는 파일도 완성본은 한 곳에만 둔다.
OWNERS = {
    "00": ["go.mod", "Makefile", ".gitignore", ".env.example", "docker-compose.yml", "scripts/verify.sh",
           "internal/config/config.go", "internal/config/config_test.go", "internal/id/id.go", "internal/id/id_test.go",
           "internal/adapter/pgstore/store.go", "internal/adapter/pgstore/migrations/0001_init.sql",
           "cmd/server/main.go", "cmd/server/main_test.go", "cmd/server/panic_test.go", "web/embed.go", "web/assets/index.html"],
    "01": ["internal/adapter/pgstore/migrations/0003_message_time.sql", "internal/adapter/pgstore/messages.go",
           "internal/adapter/pgstore/messages_test.go", "internal/app/app.go", "internal/app/message.go",
           "internal/app/app_test.go"],
    "02": ["internal/adapter/realtime/protocol.go", "internal/adapter/realtime/hub.go", "internal/adapter/realtime/hub_test.go",
           "internal/adapter/realtime/hint_test.go", "internal/adapter/realtime/conn.go", "internal/adapter/realtime/resync.go",
           "internal/adapter/realtime/resync_test.go", "internal/adapter/realtime/resync_internal_test.go",
           "internal/adapter/realtime/session_test.go", "internal/adapter/realtime/revalidate_test.go",
           "internal/adapter/realtime/dbtimeout_test.go", "internal/adapter/realtime/dbstep_test.go", "internal/adapter/realtime/errtrace_test.go", "internal/adapter/realtime/retry_test.go",
           "internal/adapter/pgstore/listener.go", "internal/adapter/pgstore/listener_test.go",
           "internal/testsupport/testsupport.go"],
    "03": ["internal/auth/auth.go", "internal/auth/auth_test.go", "internal/adapter/pgstore/sessions.go", "internal/adapter/pgstore/sessions_test.go",
           "internal/adapter/httpapi/httpapi.go", "internal/adapter/httpapi/response.go", "internal/adapter/httpapi/session.go",
           "internal/adapter/httpapi/revalidate_internal_test.go", "internal/adapter/httpapi/errlog_internal_test.go", "internal/adapter/httpapi/errcode_internal_test.go", "internal/app/logging_internal_test.go",
           "internal/adapter/httpapi/conversation.go", "internal/adapter/httpapi/e2e_test.go", "internal/adapter/httpapi/pages_test.go",
           "internal/adapter/httpapi/input_test.go", "internal/adapter/httpapi/channel_test.go",
           # 토큰 보안 묶음(D46): 유휴 만료·WebSocket 티켓·세션 종료 알림·보안 헤더·HTML 인라인 스크립트 검사.
           "internal/adapter/pgstore/migrations/0018_sec_sessions.sql", "internal/adapter/pgstore/sessionsec.go",
           # 익명 고객 세션은 만료되지 않는다(D46 고침): expires_at 이 비면 절대 만료가 없다.
           "internal/adapter/pgstore/migrations/0019_customer_sessions_forever.sql", "internal/adapter/pgstore/migration0019_test.go",
           "internal/adapter/pgstore/sessionsec_test.go", "internal/app/ports_session.go",
           "internal/adapter/httpapi/headers.go", "internal/adapter/httpapi/token_security_test.go", "web/html_test.go"],
    "04": ["web/assets/chat.js", "web/assets/chat.test.mjs", "web/assets/app.css", "web/assets/ui.js", "web/assets/ui.test.mjs",
           "web/assets/widget.html", "web/assets/widget.js", "web/assets/widget.test.mjs",
           "web/assets/widget.css", "web/assets/widget-view.js", "web/assets/widget-view.test.mjs", "web/assets/icons/LICENSE", "web/assets/fonts/LICENSE",
           "web/assets/widget-screen.test.mjs",
           # 디자인 시스템(D44): SCSS 소스 → web/assets/*.css. 빌드·린트 도구와 정책 테스트.
           "package.json", "eslint.config.js", "stylelint.config.mjs", "scripts/css.mjs", "web/assets/design-system.test.mjs",
           "web/styles/app.scss", "web/styles/base/_fonts.scss", "web/styles/base/_reset.scss", "web/styles/base/_root.scss", "web/styles/base/_typography.scss", "web/styles/base/_utilities.scss", "web/styles/components/_attachment.scss", "web/styles/components/_avatar.scss", "web/styles/components/_button.scss", "web/styles/components/_chip.scss", "web/styles/components/_composer.scss", "web/styles/components/_form.scss", "web/styles/components/_message.scss", "web/styles/components/_navigation.scss", "web/styles/components/_overlay.scss", "web/styles/components/_surface.scss", "web/styles/components/_rating.scss", "web/styles/components/_metric.scss", "web/styles/layouts/_desk.scss", "web/styles/layouts/_settings.scss", "web/styles/pages/widget/_chat.scss", "web/styles/pages/widget/_close.scss", "web/styles/pages/widget/_composer.scss", "web/styles/pages/widget/_controls.scss", "web/styles/pages/widget/_faq.scss", "web/styles/pages/widget/_home.scss", "web/styles/pages/widget/_list.scss", "web/styles/pages/widget/_responsive.scss", "web/styles/pages/widget/_shell.scss", "web/styles/pages/widget/_tokens.scss", "web/styles/tokens/_color.scss", "web/styles/tokens/_glyph.scss", "web/styles/tokens/_index.scss", "web/styles/tokens/_scale.scss", "web/styles/tools/_index.scss", "web/styles/widget.scss"],
    "05": ["internal/adapter/pgstore/migrations/0002_reads.sql", "internal/adapter/pgstore/migrations/0005_customer_seq.sql",
           "internal/adapter/pgstore/reads.go", "internal/adapter/pgstore/reads_test.go", "internal/app/views.go",
           "internal/adapter/httpapi/live_test.go"],
    "06": ["internal/adapter/filestore/filestore.go", "internal/adapter/s3store/s3store.go", "internal/adapter/s3store/s3store_test.go",
           "internal/adapter/s3store/credentials.go", "internal/adapter/s3store/credentials_test.go",
           "internal/adapter/s3store/export_test.go",
           "internal/app/upload.go", "internal/adapter/pgstore/migrations/0012_direct_upload.sql",
           "internal/adapter/pgstore/migrations/0014_attachment_lease.sql",
           "internal/adapter/pgstore/migrations/0015_attachment_lease_token.sql",
           "internal/adapter/pgstore/attachments.go", "internal/adapter/pgstore/attachments_test.go",
           "internal/adapter/httpapi/attachment.go", "internal/adapter/httpapi/upload_test.go",
           "internal/adapter/filestore/filestore_test.go", "internal/adapter/httpapi/attachment_policy_test.go",
           "internal/adapter/httpapi/upload_race_test.go", "internal/adapter/httpapi/upload_lease_test.go",
           "internal/adapter/httpapi/upload_owner_test.go", "internal/app/upload_internal_test.go",
           "internal/adapter/httpapi/video_test.go"],
    "07": ["internal/adapter/pgstore/migrations/0004_channel_features.sql", "internal/adapter/pgstore/migrations/0008_waiting.sql",
           "internal/adapter/pgstore/conversations.go",
           "internal/adapter/pgstore/conversations_test.go", "internal/adapter/pgstore/workspace.go",
           "internal/app/conversation.go", "internal/app/workspace.go", "internal/adapter/httpapi/agent.go",
           "internal/adapter/httpapi/ops_test.go", "web/assets/desk.html", "web/assets/desk.js",
           "web/assets/desk-screen.test.mjs"],
    "08": ["internal/adapter/pgstore/migrations/0006_product.sql", "internal/adapter/pgstore/team.go", "internal/adapter/pgstore/team_test.go",
           "internal/adapter/pgstore/settings.go", "internal/app/settings.go", "internal/app/settings_test.go",
           "internal/app/team.go", "internal/app/member.go", "internal/app/member_test.go",
           "internal/adapter/httpapi/admin.go", "internal/adapter/httpapi/ratelimit.go", "internal/adapter/httpapi/ratelimit_test.go",
           "internal/adapter/httpapi/product_test.go", "web/assets/embed.js", "web/assets/embed.test.mjs", "Dockerfile", ".dockerignore",
           "internal/core/faq.go", "internal/app/ports_manage.go", "internal/app/manage.go",
           "internal/adapter/pgstore/migrations/0016_widget_faq.sql", "internal/adapter/pgstore/manage.go",
           "internal/adapter/pgstore/manage_test.go", "internal/adapter/httpapi/manage.go",
           "internal/adapter/httpapi/manage_test.go", "web/assets/faq.test.mjs"],
    "09": ["internal/adapter/ollama/ollama.go", "internal/adapter/ollama/ollama_test.go", "internal/app/draft.go", "internal/adapter/httpapi/draft_test.go"],
    "10": ["cmd/loadtest/main.go", "cmd/server/shutdown_test.go", ".golangci.yml",
           # 배포와 DB 떼기(D49): 마이그레이션은 배포의 앞 단계(server migrate), 서버 기동은 읽기만, 마이그레이션 린트·해시 고정.
           "cmd/server/commands.go", "cmd/server/deploydb_test.go", "internal/app/ports_schema.go",
           "internal/adapter/pgstore/migrate.go", "internal/adapter/pgstore/migrate_test.go",
           "internal/adapter/pgstore/migrationlint_test.go", "internal/adapter/pgstore/migrations.sum"],
    "12": ["internal/adapter/pgstore/migrations/0009_history.sql", "internal/adapter/pgstore/migrations/0010_conversation_per_request.sql", "internal/adapter/pgstore/migrations/0011_past_conversations.sql", "internal/adapter/pgstore/history.go", "internal/adapter/pgstore/history_test.go",
           "internal/adapter/httpapi/history_test.go", "internal/adapter/httpapi/leak_test.go", "internal/adapter/httpapi/revoke_test.go",
           # 상담 지표와 만족도 조사(D45): 응답·종료 기록과 만족도, 지표 집계.
           "internal/adapter/pgstore/migrations/0017_metrics.sql", "internal/core/metrics.go", "internal/app/ports_metrics.go", "internal/app/ports_clock.go",
           "internal/app/metrics.go", "internal/adapter/pgstore/metrics.go", "internal/adapter/pgstore/metrics_test.go",
           "internal/adapter/pgstore/export_test.go", "internal/adapter/httpapi/metrics.go", "internal/adapter/httpapi/metrics_test.go",
           # 고객이 상담을 직접 종료(D48): 누가 닫았나(0020), 고객 종료 포트·유스케이스, 겹침 테스트, 위젯의 종료 칸 스타일.
           "internal/adapter/pgstore/migrations/0020_customer_close.sql", "internal/app/ports_close.go", "internal/app/close.go",
           "internal/adapter/pgstore/close.go", "internal/adapter/pgstore/close_test.go", "internal/adapter/httpapi/close_test.go"],
    "13": ["internal/app/maintenance.go", "internal/app/input.go", "internal/app/live_visible_test.go",
           "internal/core/page.go", "internal/adapter/pgstore/locks.go", "internal/adapter/pgstore/locks_test.go",
           "internal/adapter/httpapi/harness_test.go", "internal/adapter/httpapi/entry_test.go",
           "internal/adapter/httpapi/live_visibility_test.go", "internal/adapter/httpapi/subscribe_race_test.go",
           "internal/adapter/httpapi/page_limits_test.go", "internal/adapter/httpapi/email_test.go",
           "internal/app/deleted_view_test.go",
           "internal/core/doc.go", "internal/core/conversation.go", "internal/core/message.go", "internal/core/read.go",
           "internal/core/identity.go", "internal/core/team.go", "internal/core/directory.go", "internal/core/workspace.go",
           "internal/core/history.go", "internal/core/attachment.go", "internal/core/files.go", "internal/core/signal.go",
           "internal/core/errors.go", "internal/app/doc.go", "internal/app/ports.go", "internal/app/ports_draft.go",
           "internal/app/session.go", "internal/archtest/arch_test.go", "internal/archtest/logerr_test.go", "internal/archtest/loginput_test.go", "internal/archtest/alarmfilter_test.go", "internal/archtest/invalidcode_test.go",
           "internal/logctx/logctx.go", "internal/logctx/handler.go", "internal/logctx/trace.go",
           "internal/logctx/logctx_test.go", "internal/logctx/errkind_test.go", "internal/app/logging.go", "internal/adapter/httpapi/logging.go",
           "internal/adapter/httpapi/logging_test.go", "internal/adapter/httpapi/logging_internal_test.go",
           "internal/telemetry/telemetry.go", "internal/telemetry/setup.go", "internal/telemetry/metrics.go", "internal/telemetry/panic.go",
           "internal/telemetry/redact.go", "internal/telemetry/client.go", "internal/telemetry/telemetry_test.go",
           "internal/adapter/pgstore/telemetry.go", "internal/adapter/pgstore/telemetry_test.go", "internal/adapter/httpapi/telemetry.go",
           "internal/adapter/httpapi/telemetry_test.go", "internal/adapter/s3store/telemetry_test.go",
           "internal/archtest/testdata/otelimport/usecase.go", "internal/archtest/testdata/logleak/leak.go", "internal/archtest/testdata/loginput/leak.go",
           "cmd/server/startup_test.go"],
    "11": ["internal/adapter/pgstore/migrations/0007_workspaces.sql", "internal/adapter/pgstore/workspaces.go",
           "internal/adapter/pgstore/workspaces_test.go", "internal/app/workspace_admin.go",
           "internal/adapter/httpapi/workspace.go", "internal/adapter/httpapi/workspace_test.go", "internal/adapter/httpapi/tenant_test.go",
           "internal/adapter/pgstore/migrations/0013_roles.sql", "internal/app/permission.go",
           "internal/adapter/pgstore/roles_test.go", "internal/adapter/httpapi/roles_test.go",
           "internal/adapter/httpapi/inbox_roles_test.go"],
}
LANG = {".go": "go", ".sql": "sql", ".js": "js", ".mjs": "js", ".css": "css", ".scss": "scss", ".json": "json", ".html": "html",
        ".yml": "yaml", ".sh": "bash", ".mod": "go"}
# package-lock.json 은 go.sum 처럼 도구가 만드는 잠금 파일이라 가이드에 싣지 않는다.
SKIP_SUFFIX = (".md", ".png", ".svg", ".woff2", ".log", "go.sum", "package-lock.json", "guide-embed.py")


EXCLUDE_DIRS = ("bin/", "data/", ".idea/", "node_modules/", "docs/", "deploy/", ".github/")


def git_files():
    """git 이 아는 파일(추적 중 + 무시되지 않은 새 파일). git 이 없거나 리포가 아니면(.git 없는 사본, 압축본) None."""
    try:
        r = subprocess.run(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
                           cwd=ROOT, capture_output=True, text=True)
    except OSError:
        return None
    # 상위 디렉토리의 다른 리포를 잡지 않게, 이 리포의 최상위가 ROOT 인지도 본다.
    top = subprocess.run(["git", "rev-parse", "--show-toplevel"], cwd=ROOT, capture_output=True, text=True)
    if r.returncode != 0 or top.returncode != 0 or os.path.realpath(top.stdout.strip()) != os.path.realpath(ROOT):
        return None
    return [p for p in r.stdout.split("\0") if p]


# ALWAYS_IGNORED 는 .gitignore 와 상관없이 세지 않는 파일이다. 운영체제·편집기가 만든다.
ALWAYS_IGNORED = (".DS_Store", "Thumbs.db", "*.swp", "*~")


def ignore_patterns():
    """.gitignore 의 패턴(주석·빈 줄·부정(!) 제외)과 ALWAYS_IGNORED."""
    pats = list(ALWAYS_IGNORED)
    try:
        with open(os.path.join(ROOT, ".gitignore"), encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if line and not line.startswith(("#", "!")):
                    pats.append(line)
    except FileNotFoundError:
        pass
    return pats


def ignored(path, pats):
    """git 의 무시 규칙을 단순하게 따른다. / 로 시작하면 리포 최상위 기준, / 로 끝나면 디렉토리, 아니면 어느 깊이의 이름이든."""
    parts = path.split("/")
    for pat in pats:
        anchored, is_dir = pat.startswith("/"), pat.endswith("/")
        p = pat.strip("/")
        if anchored:
            target = "/".join(parts[: p.count("/") + 1])
            if fnmatch.fnmatchcase(target, p) and (not is_dir or len(parts) > p.count("/") + 1):
                return True
            continue
        names = parts[:-1] if is_dir else parts
        if "/" in p:
            if fnmatch.fnmatchcase(path, p) or any(fnmatch.fnmatchcase("/".join(parts[i:]), p) for i in range(len(parts))):
                return True
        elif any(fnmatch.fnmatchcase(n, p) for n in names):
            return True
    return False


def walk_files():
    """git 없이 디렉토리를 훑는다. .git 과 무시 패턴에 걸리는 것은 뺀다."""
    pats = ignore_patterns()
    out = []
    for dirpath, dirnames, filenames in os.walk(ROOT):
        rel = os.path.relpath(dirpath, ROOT)
        rel = "" if rel == "." else rel.replace(os.sep, "/") + "/"
        dirnames[:] = sorted(d for d in dirnames if d != ".git" and not (rel + d + "/").startswith(EXCLUDE_DIRS)
                             and not ignored(rel + d + "/x", pats))
        out += [rel + f for f in filenames if not ignored(rel + f, pats)]
    return out


def all_sources():
    """리포의 파일 목록. git 이 아는 파일(추적 중 + 무시되지 않은 새 파일)만 센다 — .DS_Store 처럼 git 이 무시하는 파일이
    리포 안에 있어도 --check 가 "OWNERS 에 없다" 로 실패하지 않게. 지운 파일(색인에만 남은 것)은 뺀다.
    git 이 없으면(.git 없는 사본) 디렉토리를 훑고 .gitignore 와 ALWAYS_IGNORED 로 거른다(감사 16 — 전에는 git 오류로 멈췄다)."""
    listed = git_files()
    if listed is None:
        listed = walk_files()
    files = {p for p in listed if os.path.isfile(os.path.join(ROOT, p))}
    return sorted(p for p in files if not p.startswith(EXCLUDE_DIRS) and not p.endswith(SKIP_SUFFIX))


def lang(path):
    base = os.path.basename(path)
    if base in ("Dockerfile", ".dockerignore", ".gitignore", ".env.example"):
        return "dockerfile" if base == "Dockerfile" else "text"
    if base == "Makefile":
        return "makefile"
    return LANG.get(os.path.splitext(path)[1], "text")


def tree(paths, label=None):
    """파일 목록을 디렉토리 트리 문자열로."""
    root = {}
    for p in paths:
        node = root
        for part in p.split("/"):
            node = node.setdefault(part, {})
    lines = []

    def walk(node, prefix, path):
        items = sorted(node.items(), key=lambda kv: (not kv[1], kv[0]))  # 디렉토리 먼저
        for i, (name, kids) in enumerate(items):
            last = i == len(items) - 1
            full = f"{path}/{name}" if path else name
            tag = f"   ← {label[full]}" if label and full in label else ""
            lines.append(f"{prefix}{'└── ' if last else '├── '}{name}{tag}")
            walk(kids, prefix + ("    " if last else "│   "), full)

    walk(root, "", "")
    return "\n".join(lines)


def section(step, files):
    parts = [START, "", "## 정답지 — 이 단계에서 완성되는 파일 전문", "",
             "위를 다 읽고 **직접 써 본 뒤에** 연다. 생성된 구간이라 손으로 고치지 말고 코드를 고친 뒤 "
             "`python3 scripts/guide-embed.py` 를 돌린다.", "", "```", tree(files), "```", ""]
    for f in files:
        with open(os.path.join(ROOT, f), encoding="utf-8") as fh:
            body = fh.read().rstrip("\n")
        n = body.count("\n") + 1
        parts += [f"### `{f}` ({n}줄)", "", f"```{lang(f)}", body, "```", ""]
    parts.append(END)
    return "\n".join(parts)


def splice(doc, block):
    if START in doc and END in doc:
        pre = doc[: doc.index(START)]
        post = doc[doc.index(END) + len(END):]
        return pre + block + post
    return doc.rstrip("\n") + "\n\n" + block + "\n"


def main():
    check = "--check" in sys.argv[1:]
    if not os.path.isdir(GUIDE):
        # docs/ 는 리포에 올리지 않는다(.gitignore). 가이드가 없는 클론에서는 맞춰 볼 정답지가 없다.
        print("가이드 디렉토리가 없어 건너뛴다(docs/ 는 로컬 전용)")
        return
    sources = all_sources()
    owned = {f: step for step, fs in OWNERS.items() for f in fs}
    missing = [f for f in sources if f not in owned]
    gone = [f for f in owned if f not in sources]
    if missing or gone:
        print("매핑이 코드와 어긋난다.", file=sys.stderr)
        for f in missing:
            print(f"  단계가 없는 파일: {f}", file=sys.stderr)
        for f in gone:
            print(f"  없는 파일을 가리킴: {f}", file=sys.stderr)
        sys.exit(1)

    total = 0
    stale = []

    def put(path, doc, out):
        """check 면 다른지만 적고, 아니면 쓴다."""
        if check:
            if out != doc:
                stale.append(os.path.relpath(path, ROOT))
            return
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(out)

    for step, files in OWNERS.items():
        name = next(n for n in os.listdir(GUIDE) if n.startswith(step + "-"))
        path = os.path.join(GUIDE, name)
        with open(path, encoding="utf-8") as fh:
            doc = fh.read()
        block = section(step, files)
        put(path, doc, splice(doc, block))
        n = block.count("\n")
        total += n
        if not check:
            print(f"{name}: 파일 {len(files)}개, {n}줄")

    # INDEX — 전체 트리에 단계 표식
    label = owned
    idx = os.path.join(GUIDE, "INDEX.md")
    with open(idx, encoding="utf-8") as fh:
        doc = fh.read()
    block = "\n".join([START, "", "## 패키지 구조 — 파일마다 어느 단계에서 완성되나", "",
                       "```", tree(sources, label), "```", "", END])
    put(idx, doc, splice(doc, block))
    if check:
        if stale:
            print("가이드의 정답지가 코드와 다르다. `python3 scripts/guide-embed.py` 를 돌린다:", file=sys.stderr)
            for f in stale:
                print(f"  {f}", file=sys.stderr)
            sys.exit(1)
        print(f"가이드 정답지가 코드와 같다(단계 {len(OWNERS)}개, 파일 {len(sources)}개)")
        return
    print(f"INDEX.md: 트리 {len(sources)}개 파일. 합계 {total}줄 심음")


if __name__ == "__main__":
    main()
