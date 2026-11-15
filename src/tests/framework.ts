/** 极简测试运行器：node:assert + 用例注册，输出 TAP 风格 */
import { inspect } from 'node:util';

type Fn = () => void | Promise<void>;

interface TestCase {
  name: string;
  fn: Fn;
}

const cases: TestCase[] = [];
let currentSuite = '';

export function suite(name: string, fn: () => void): void {
  currentSuite = name;
  fn();
  currentSuite = '';
}

export function test(name: string, fn: Fn): void {
  cases.push({ name: currentSuite ? `${currentSuite} › ${name}` : name, fn });
}

export async function runAll(): Promise<number> {
  let passed = 0;
  const failures: Array<{ name: string; error: unknown }> = [];
  for (const c of cases) {
    const started = Date.now();
    try {
      await c.fn();
      passed++;
      console.log(`  ✓ ${c.name} (${Date.now() - started}ms)`);
    } catch (err) {
      failures.push({ name: c.name, error: err });
      console.log(`  ✗ ${c.name}`);
      console.log(`      ${inspect(err, { depth: 4, colors: false }).split('\n').join('\n      ')}`);
    }
  }
  console.log(`\n${passed}/${cases.length} 通过`);
  if (failures.length) {
    console.log('\n失败用例：');
    for (const f of failures) console.log(`  - ${f.name}`);
  }
  return failures.length === 0 ? 0 : 1;
}

export function reset(): void {
  cases.length = 0;
}
