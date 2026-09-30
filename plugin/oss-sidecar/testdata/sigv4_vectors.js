// 用 aws4（Node 侧独立实现）生成 SigV4 基准向量，与 botocore 交叉验证。
// 两者若对同一请求给出同一签名，向量才可信 —— 不凭记忆写常量。
const aws4 = require('aws4');

const AK = 'AKIAIOSFODNN7EXAMPLE';
const SK = 'wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY';
const DT = '20130524T000000Z';

function vec(name, opts) {
  const signer = new aws4.RequestSigner(opts, { accessKeyId: AK, secretAccessKey: SK });
  signer.prepareRequest();
  console.log('=== ' + name);
  console.log('CANON <<<' + signer.canonicalString() + '>>>');
  console.log('STS   <<<' + signer.stringToSign() + '>>>');
  console.log('SIG   ' + signer.signature());
  console.log();
}

// 与 botocore 的 "GET space+query" 同一请求：签名必须一致。
// 注意 aws4 只从 path 里解析 query，传 opts.query 会被忽略（第一次就踩了，签名对不上别急着改实现）。
vec('header GET space+query', {
  service: 's3', region: 'us-east-1', host: 'mybucket.s3.amazonaws.com',
  path: '/a/b c.txt?prefix=somePrefix&marker=someMarker&max-keys=2', method: 'GET',
  headers: { 'X-Amz-Date': DT },
});

// 预签名：X-Amz-Date 也得走 path 里的 query 才能固定住
vec('presign GET', {
  service: 's3', region: 'us-east-1', host: 'mybucket.s3.amazonaws.com',
  path: '/test.txt?X-Amz-Date=' + DT, method: 'GET', signQuery: true,
  headers: {},
});

vec('presign GET cjk', {
  service: 's3', region: 'us-east-1', host: 'mybucket.s3.amazonaws.com',
  path: '/目录/中文 文件.txt?X-Amz-Date=' + DT, method: 'GET', signQuery: true,
  headers: {},
});
