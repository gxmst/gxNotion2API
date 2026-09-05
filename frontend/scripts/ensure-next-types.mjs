import { mkdir, access, writeFile } from 'node:fs/promises';
import path from 'node:path';

const root = process.cwd();
const files = [
  ['.next', 'types', 'routes.d.ts'],
  ['.next', 'types', 'cache-life.d.ts'],
  ['.next', 'types', 'app', 'layout.ts'],
  ['.next', 'types', 'app', 'page.ts'],
  ['.next', 'types', 'package.json'],
];

async function ensureFile(parts) {
  const target = path.join(root, ...parts);
  await mkdir(path.dirname(target), { recursive: true });
  try {
    await access(target);
  } catch {
    const content = target.endsWith('package.json')
      ? '{"type":"module"}\n'
      : 'export {};\n';
    await writeFile(target, content, 'utf8');
  }
}

await Promise.all(files.map(ensureFile));
