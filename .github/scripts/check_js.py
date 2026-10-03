#!/usr/bin/env python3
"""node --check для JS страницы: и отдельных .js, и встроенных <script> в .html.

Статика вшита в бинарник web (go:embed), поэтому синтаксическая ошибка в скрипте
дошла бы до браузера молча — страница просто не заработает.

  check_js.py internal/webapp/static
"""
import os
import re
import subprocess
import sys
import tempfile

root = sys.argv[1]
failed = checked = 0
for name in sorted(os.listdir(root)):
    path = os.path.join(root, name)
    if name.endswith(".js"):
        sources = [(name, open(path, encoding="utf-8").read())]
    elif name.endswith(".html"):
        html = open(path, encoding="utf-8").read()
        # только встроенные скрипты: <script src=…> проверяется своим файлом
        sources = [(f"{name} <script #{i + 1}>", m.group(1))
                   for i, m in enumerate(re.finditer(r"<script(?![^>]*\bsrc=)[^>]*>(.*?)</script>", html, re.S))]
    else:
        continue
    for label, code in sources:
        with tempfile.NamedTemporaryFile("w", suffix=".js", delete=False, encoding="utf-8") as f:
            f.write(code)
        r = subprocess.run(["node", "--check", f.name], capture_output=True, text=True)
        os.unlink(f.name)
        checked += 1
        if r.returncode != 0:
            failed += 1
            print(f"::error title=js::{label}\n{r.stderr}")
print(f"проверено скриптов: {checked}, с ошибками: {failed}")
sys.exit(1 if failed or not checked else 0)
