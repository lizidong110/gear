"""用 botocore（AWS 官方实现）生成 SigV4 基准向量，供 Go 侧单测作黄金值。

不是凭记忆写常量：这里每一步都由 botocore 现算，Go 侧必须逐个对得上。
"""
import hashlib

from botocore.auth import SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.credentials import Credentials

AK = "AKIAIOSFODNN7EXAMPLE"
SK = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
DT = "20130524T000000Z"
REGION = "us-east-1"
EMPTY_SHA = hashlib.sha256(b"").hexdigest()


def vec(name, method, url, body=b"", payload=None, extra_headers=None):
    host = url.split("//", 1)[1].split("/", 1)[0]
    h = {"host": host, "x-amz-date": DT}
    # S3 实际调用必带 x-amz-content-sha256，向量也要带上，否则签出来的头跟实现不一致。
    h["x-amz-content-sha256"] = payload if payload is not None else hashlib.sha256(body).hexdigest()
    if extra_headers:
        h.update(extra_headers)
    req = AWSRequest(method=method, url=url, headers=h, data=body)
    auth = SigV4Auth(Credentials(AK, SK), "s3", REGION)
    cr = auth.canonical_request(req)
    req.datetime = DT
    req.context.setdefault("timestamp", DT)
    sts = auth.string_to_sign(req, cr)
    sig = auth.signature(sts, req)
    print("=== %s" % name)
    print("URL: %s %s" % (method, url))
    print("CANON <<<%s>>>" % cr)
    print("STS   <<<%s>>>" % sts)
    print("SIG   %s" % sig)
    print()


# 1) GET 对象，路径含空格（锁 URI 编码行为），带查询串
vec("GET simple", "GET", "https://mybucket.s3.amazonaws.com/test.txt")
vec("GET space+query", "GET",
    "https://mybucket.s3.amazonaws.com/a/b c.txt?prefix=somePrefix&marker=someMarker&max-keys=2")
# 2) PUT 带 body
vec("PUT body", "PUT", "https://mybucket.s3.amazonaws.com/a/b c.txt", body=b"hello world")
# 3) presign：payload 用 UNSIGNED-PAYLOAD
vec("PRESIGN unsigned", "GET", "https://mybucket.s3.amazonaws.com/test.txt",
    payload="UNSIGNED-PAYLOAD")
print("EMPTY_SHA =", EMPTY_SHA)
print("sha256('hello world') =", hashlib.sha256(b"hello world").hexdigest())
print("sha256('UNSIGNED-PAYLOAD') =", hashlib.sha256(b"UNSIGNED-PAYLOAD").hexdigest())
print()
# 4) CJK key + 空格：锁 URI 编码行为（不许双重编码，也不许原样留中文）
vec("GET cjk", "GET", "https://mybucket.s3.amazonaws.com/目录/中文 文件.txt")
