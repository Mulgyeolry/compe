import os
import subprocess
import urllib.request


def main() -> None:
    with urllib.request.urlopen(
        "http://searxng:8080/search?q=computer+competition&format=json",
        timeout=30,
    ) as response:
        content_type = response.headers.get("content-type", "")
        if response.status != 200 or "application/json" not in content_type:
            raise RuntimeError("SearXNG JSON endpoint is not ready")
    print("SEARXNG_JSON=True")

    notify_url = os.environ.get("APPRISE_STATELESS_URLS", "")
    if not notify_url:
        raise RuntimeError("APPRISE_STATELESS_URLS is empty")
    result = subprocess.run(
        [
            "apprise",
            "-v",
            "-t",
            "比赛资讯助手：云服务器测试",
            "-b",
            "如果你收到这封邮件，说明云端部署的 SMTP 已正常工作。",
            notify_url,
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError("Apprise email test failed")
    print("CLOUD_EMAIL_TEST=True")


if __name__ == "__main__":
    main()
