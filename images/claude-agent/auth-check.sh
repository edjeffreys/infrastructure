#!/usr/bin/env bash
set -euo pipefail

claude auth status --json | node -e '
  let s = "";
  process.stdin.on("data", d => s += d).on("end", () => {
    const st = JSON.parse(s);
    if (!st.loggedIn) { console.log("not logged in"); process.exit(1); }
    const days = (Date.parse(st.loginExpiresAt) - Date.now()) / 86400e3;
    if (days < Number(process.env.LOGIN_WARN_DAYS || 3)) {
      console.log(`login expires ${st.loginExpiresAt}`); process.exit(1);
    }
  });
'
