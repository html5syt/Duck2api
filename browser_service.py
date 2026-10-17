#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Duck2api 浏览器挑战解决伴生服务 (Browser Challenge Solver Service)
使用真实 Chromium 浏览器实例 (通过 Playwright) 处理 DuckDuckGo JSA 反爬挑战。
架构：单线程 asyncio 事件循环 + async_playwright + 内置极简异步 HTTP 服务器。
"""

import os
import sys
import json
import asyncio
from playwright.async_api import async_playwright

PORT = int(os.environ.get("BROWSER_SERVICE_PORT", 8787))
PROXY_URL = os.environ.get("PROXY_URL", "")

def find_system_browser():
    candidates = [
        os.environ.get("CHROMIUM_PATH", ""),
        os.environ.get("CHROME_PATH", ""),
        r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe",
        r"C:\Program Files\Microsoft\Edge\Application\msedge.exe",
        r"C:\Program Files\Google\Chrome\Application\chrome.exe",
        r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
        "/usr/bin/chromium-browser",
        "/usr/bin/chromium",
        "/usr/bin/google-chrome",
        "/usr/bin/google-chrome-stable",
    ]
    for c in candidates:
        if c and os.path.exists(c):
            return c
    return None

class AsyncBrowserWorker:
    def __init__(self):
        self.playwright = None
        self.browser = None
        self.context = None
        self.page = None
        self.ready = False
        self.init_event = asyncio.Event()

    async def start(self):
        try:
            self.playwright = await async_playwright().start()
            exe = find_system_browser()
            launch_args = [
                "--disable-blink-features=AutomationControlled",
                "--no-sandbox",
                "--disable-dev-shm-usage",
                "--disable-gpu"
            ]
            kwargs = {
                "headless": True,
                "args": launch_args
            }
            if exe:
                kwargs["executable_path"] = exe
            if PROXY_URL:
                kwargs["proxy"] = {"server": PROXY_URL}

            self.browser = await self.playwright.chromium.launch(**kwargs)
            self.context = await self.browser.new_context(
                user_agent="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
            )
            self.page = await self.context.new_page()
            print("[BrowserWorker] Pre-warming page https://duck.ai/...", file=sys.stderr)
            try:
                await self.page.goto("https://duck.ai/", wait_until="domcontentloaded", timeout=25000)
            except Exception as e:
                print(f"[BrowserWorker] Pre-warm navigation notice: {e}", file=sys.stderr)
            self.ready = True
            self.init_event.set()
            print("[BrowserWorker] Real Chromium browser instance initialized and ready.", file=sys.stderr)
        except Exception as e:
            print(f"[BrowserWorker] Start error: {e}", file=sys.stderr)
            self.init_event.set()

    async def wait_ready(self, timeout=30):
        if self.ready:
            return True
        try:
            await asyncio.wait_for(self.init_event.wait(), timeout)
            return self.ready
        except asyncio.TimeoutError:
            return False

    async def solve_challenge(self, js_code):
        if not await self.wait_ready(timeout=30):
            raise RuntimeError("Chromium browser instance not ready")

        in_page_js = f"""
        async () => {{
            const rawEval = await (async function() {{
                return {js_code};
            }})();

            if (!rawEval || typeof rawEval !== "object" || !Array.isArray(rawEval.client_hashes)) {{
                throw new Error("Challenge script did not return an object with client_hashes");
            }}

            const hashed = await Promise.all(rawEval.client_hashes.map(async function(v) {{
                const enc = new TextEncoder().encode(String(v));
                const digest = await crypto.subtle.digest("SHA-256", enc);
                const bytes = new Uint8Array(digest);
                let bin = "";
                for (let i = 0; i < bytes.length; i++) {{
                    bin += String.fromCharCode(bytes[i]);
                }}
                return btoa(bin);
            }}));

            const meta = Object.assign({{}}, rawEval.meta || {{}}, {{
                origin: window.location.origin || "https://duck.ai",
                stack: "Error\\nat l (" + window.location.origin + "/dist/duckai-dist/entry.duckai.295750b5de73538d3230.js:2:2093850)\\nat async " + window.location.origin + "/dist/duckai-dist/entry.duckai.295750b5de73538d3230.js:2:1834378",
                duration: "100"
            }});

            const result = Object.assign({{}}, rawEval, {{
                client_hashes: hashed,
                meta: meta
            }});

            return JSON.stringify(result);
        }}
        """
        return await self.page.evaluate(in_page_js)

    async def close(self):
        if self.browser:
            try: await self.browser.close()
            except: pass
        if self.playwright:
            try: await self.playwright.stop()
            except: pass
        self.ready = False

worker = AsyncBrowserWorker()

async def handle_client(reader, writer):
    try:
        # 读取 HTTP 请求行
        line = await reader.readline()
        if not line:
            writer.close()
            return
        req_line = line.decode("utf-8").strip()
        parts = req_line.split(" ")
        if len(parts) < 2:
            writer.close()
            return
        method, path = parts[0], parts[1]

        # 读取 HTTP 头部
        content_length = 0
        while True:
            header_line = await reader.readline()
            if not header_line or header_line == b"\r\n" or header_line == b"\n":
                break
            h = header_line.decode("utf-8").strip()
            if h.lower().startswith("content-length:"):
                content_length = int(h.split(":", 1)[1].strip())

        # 路由分发
        if method == "GET" and path == "/health":
            body = json.dumps({"status": "ok", "ready": worker.ready}).encode("utf-8")
            resp = (
                b"HTTP/1.1 200 OK\r\n"
                b"Content-Type: application/json\r\n"
                b"Content-Length: " + str(len(body)).encode("ascii") + b"\r\n"
                b"Connection: close\r\n\r\n" + body
            )
            writer.write(resp)
            await writer.drain()
            writer.close()
            return

        if method == "POST" and path == "/solve":
            req_body_bytes = await reader.readexactly(content_length) if content_length > 0 else b""
            try:
                data = json.loads(req_body_bytes.decode("utf-8"))
                js_code = data.get("js_code", "")
                if not js_code:
                    raise ValueError("missing js_code parameter")
                payload = await worker.solve_challenge(js_code)
                resp_data = json.dumps({"payload": payload}).encode("utf-8")
                resp = (
                    b"HTTP/1.1 200 OK\r\n"
                    b"Content-Type: application/json\r\n"
                    b"Content-Length: " + str(len(resp_data)).encode("ascii") + b"\r\n"
                    b"Connection: close\r\n\r\n" + resp_data
                )
                writer.write(resp)
                await writer.drain()
            except Exception as e:
                err_data = json.dumps({"error": str(e)}).encode("utf-8")
                resp = (
                    b"HTTP/1.1 500 Internal Server Error\r\n"
                    b"Content-Type: application/json\r\n"
                    b"Content-Length: " + str(len(err_data)).encode("ascii") + b"\r\n"
                    b"Connection: close\r\n\r\n" + err_data
                )
                writer.write(resp)
                await writer.drain()
            writer.close()
            return

        # 404
        writer.write(b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        await writer.drain()
        writer.close()
    except Exception as e:
        try: writer.close()
        except: pass

async def main():
    server = await asyncio.start_server(handle_client, "127.0.0.1", PORT)
    print(f"[BrowserService] Serving async solver on 127.0.0.1:{PORT}", file=sys.stderr)
    asyncio.create_task(worker.start())

    async with server:
        await server.serve_forever()

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass
