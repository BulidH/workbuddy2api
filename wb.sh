#!/usr/bin/env bash
# wb.sh — WorkBuddy2API 日常管理助手
#
#   ./wb.sh status      健康检查 + 账号池状态
#   ./wb.sh panel       在浏览器打开管理面板（推荐，加号/看状态都在里面）
#   ./wb.sh logs        跟踪日志
#   ./wb.sh start|stop|restart
#   ./wb.sh models      列出可用模型
#   ./wb.sh chat <模型> <内容>   快速对话测试
#   ./wb.sh add         交互式添加一个新账号（再跑一次 OAuth 登录）
#   ./wb.sh key         打印当前 api_key
set -euo pipefail
cd "$(dirname "$0")"

CONTAINER="workbuddy2api"
BASE="${WB_BASE:-http://127.0.0.1:7863}"

api_key() { python3 -c "import json;print(json.load(open('config.json')).get('api_key',''))"; }
auth() { echo "Authorization: Bearer $(api_key)"; }

cmd_status() {
    echo "--- /healthz ---"
    curl -s -w "\nHTTP %{http_code}\n" "$BASE/healthz"
    echo "--- /status ---"
    curl -s "$BASE/status" -H "$(auth)" | python3 -m json.tool 2>/dev/null \
        || echo "（取不到 status，检查 api_key / 容器是否在跑）"
}

cmd_models() {
    curl -s "$BASE/v1/models" -H "$(auth)" \
        | python3 -c "import json,sys; [print(' ', m['id']) for m in json.load(sys.stdin)['data']]"
}

cmd_chat() {
    local model="${1:-global:deepseek-v4.1-flash}" msg="${2:-只回复两个字：成功}"
    curl -sN "$BASE/v1/chat/completions" -H "$(auth)" -H 'Content-Type: application/json' \
        -d "$(python3 -c "
import json,sys
print(json.dumps({'model':sys.argv[1],'messages':[{'role':'user','content':sys.argv[2]}],'stream':False}))" \
            "$model" "$msg")" \
        | python3 -c "
import json,sys
d=json.load(sys.stdin)
print('ERROR:', d) if 'error' in d else print(d['choices'][0]['message'].get('content'))"
}

cmd_add() {
    echo "在容器内启动 OAuth 登录流程（浏览器完成登录后回到终端按 y）..."
    echo
    docker exec -it "$CONTAINER" bash -lc 'cd /app && ./login.sh --realm=global' || true
    echo
    echo "重启容器以加载新账号..."
    docker compose restart
    sleep 3
    curl -s "$BASE/healthz"; echo
}

case "${1:-}" in
    status)  cmd_status ;;
    panel)
        URL="$BASE/panel/"
        echo "管理面板: $URL"
        command -v open >/dev/null 2>&1 && open "$URL" || true
        ;;
    logs)    docker compose logs -f --tail=50 ;;
    start)   docker compose up -d ;;
    stop)    docker compose down ;;
    restart) docker compose restart && sleep 3 && curl -s "$BASE/healthz" && echo ;;
    models)  cmd_models ;;
    chat)    shift; cmd_chat "$@" ;;
    add)     cmd_add ;;
    key)     api_key ;;
    *)       sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//' ;;
esac
