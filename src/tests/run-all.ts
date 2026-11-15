import { runAll } from './framework.js';

// 按顺序 import 触发用例注册
import './core.test.js';
import './events.test.js';
import './sync.test.js';
import './e2e.test.js';

const code = await runAll();
process.exit(code);
