import { cpSync, mkdirSync } from 'node:fs';
mkdirSync('dist/web', { recursive: true });
for (const f of ['index.html', 'style.css', 'app.js']) {
  cpSync(`src/web/${f}`, `dist/web/${f}`);
}
