import {createServer} from 'node:http';
import {readFile, stat} from 'node:fs/promises';
import {fileURLToPath} from 'node:url';
import {resolve, sep, extname} from 'node:path';

const root = fileURLToPath(new URL('../dist/', import.meta.url));
const port = Number(process.env.PORT ?? 8087);
const host = process.env.HOST ?? '0.0.0.0';
const types = {'.html':'text/html; charset=utf-8','.js':'text/javascript; charset=utf-8','.css':'text/css; charset=utf-8','.svg':'image/svg+xml','.png':'image/png','.woff2':'font/woff2','.ico':'image/x-icon','.txt':'text/plain; charset=utf-8','.xml':'application/xml; charset=utf-8'};

const server = createServer(async (request, response) => {
  if (request.method !== 'GET' && request.method !== 'HEAD') {
    response.writeHead(405, {Allow:'GET, HEAD'}).end();
    return;
  }
  try {
    if (request.headers.host?.split(':')[0].toLowerCase() === 'www.rigscope.dev') {
      const url = new URL(request.url, 'https://rigscope.dev');
      response.writeHead(308, {Location: 'https://rigscope.dev' + url.pathname + url.search}).end();
      return;
    }
    const pathname = decodeURIComponent(new URL(request.url, 'http://localhost').pathname);
    if (pathname.endsWith('/index.html') || pathname === '/index.html') {
      const canonical = pathname.slice(0, -'index.html'.length);
      response.writeHead(308, {Location: canonical + new URL(request.url, 'http://localhost').search}).end();
      return;
    }
    let path = resolve(root, '.' + (pathname === '/' ? '/index.html' : pathname));
    if (!path.startsWith(root.endsWith(sep) ? root : root + sep)) {
      response.writeHead(403).end('Forbidden');
      return;
    }
    let info = await stat(path);
    if (info.isDirectory()) {
      if (!pathname.endsWith('/')) {
        response.writeHead(308, {Location: pathname + '/' + new URL(request.url, 'http://localhost').search}).end();
        return;
      }
      path = resolve(path, 'index.html');
      info = await stat(path);
    }
    if (!info.isFile()) throw new Error('Not a file');
    const body = await readFile(path);
    response.writeHead(200, {
      'Content-Type':types[extname(path)] ?? 'application/octet-stream',
      'Content-Length':body.length,
      'Cache-Control':'no-cache',
      'X-Content-Type-Options':'nosniff',
      'Referrer-Policy':'strict-origin-when-cross-origin',
    });
    response.end(request.method === 'HEAD' ? undefined : body);
  } catch {
    response.writeHead(404, {'Content-Type':'text/plain; charset=utf-8'}).end('Not found');
  }
});
server.listen(port, host, () => console.log(`Rigscope website: http://${host}:${server.address().port}`));
for (const signal of ['SIGINT','SIGTERM']) process.on(signal, () => server.close());
