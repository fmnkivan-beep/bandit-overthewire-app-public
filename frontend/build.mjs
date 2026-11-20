import * as esbuild from 'esbuild';
import { mkdir, copyFile } from 'node:fs/promises';

await mkdir('dist', { recursive: true });
await copyFile('index.html', 'dist/index.html');
const options = { entryPoints: ['src/main.jsx'], bundle: true, outfile: 'dist/app.js', format: 'esm', sourcemap: true };
if (process.argv.includes('--serve')) {
  const context = await esbuild.context(options);
  await context.watch();
  const server = await context.serve({ servedir: 'dist', host: '127.0.0.1', port: 4173 });
  console.log(`Preview: http://${server.hosts[0]}:${server.port}`);
} else {
  await esbuild.build(options);
}
