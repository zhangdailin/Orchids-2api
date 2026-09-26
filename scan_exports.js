const fs = require('fs');
const path = require('path');
const { execSync } = require('child_process');

// 获取项目根目录
const rootDir = '/home/zhangdailin/Documents/Orchids-2api';

// 存储所有找到的导出
const allExports = [];

// 检查路径是否需要排除
function shouldExclude(filePath) {
    const relativePath = path.relative(rootDir, filePath);
    
    // 排除 node_modules
    if (relativePath.includes('node_modules')) return true;
    
    // 排除测试文件
    if (/\.test\.(ts|tsx|js)$/.test(relativePath)) return true;
    if (/\.spec\.(ts|tsx|js)$/.test(relativePath)) return true;
    
    // 排除 UI 组件库和测试文件
    if (relativePath.includes('/ui/')) return true;
    
    // 排除 test/spec 文件
    if (/\.test\.(ts|tsx|js)$/.test(relativePath)) return true;
    if (/\.spec\.(ts|tsx|js)$/.test(relativePath)) return true;
    
    // 排除其他特定目录 (仅排除 node_modules 和测试文件)
    if (relativePath.includes('.git/') || relativePath.includes('.npm/') ||
        relativePath.includes('venv/')) return true;
    
    return false;
}

// 递归获取文件列表
function getAllFiles(dir) {
    let results = [];
    try {
        const list = fs.readdirSync(dir);
        
        for (const file of list) {
            const filePath = path.join(dir, file);
            const stat = fs.statSync(filePath);
            
            if (stat && stat.isDirectory()) {
                const subResults = getAllFiles(filePath);
                results = results.concat(subResults);
            } else if (/\.(ts|tsx|js|jsx)$/.test(file)) {
                if (!shouldExclude(filePath)) {
                    results.push(filePath);
                }
            }
        }
    } catch (e) {
        console.error(`读取目录失败：${dir}`, e.message);
    }
    
    return results;
}

// 提取文件中的导出
function extractExports(filePath) {
    const content = fs.readFileSync(filePath, 'utf-8');
    const lines = content.split('\n');
    const relativePath = path.relative(rootDir, filePath);
    const exports = [];
    
    for (let i = 0; i < lines.length; i++) {
        const lineNum = i + 1;
        const lineContent = lines[i];
        
        // export function xxx (包括 async)
        const funcMatch = lineContent.match(/export\s+(?:async\s+)?function\s+(\w+)/);
        if (funcMatch) {
            exports.push({
                fileName: relativePath,
                functionName: funcMatch[1],
                exportType: 'named',
                line: lineNum
            });
            continue;
        }
        
        // export const/let/var xxx
        const varMatch = lineContent.match(/export\s+(const|let|var)\s+(\w+)/);
        if (varMatch) {
            exports.push({
                fileName: relativePath,
                functionName: varMatch[2],
                exportType: 'named',
                line: lineNum
            });
            continue;
        }
        
        // export class xxx
        const classMatch = lineContent.match(/export\s+class\s+(\w+)/);
        if (classMatch) {
            exports.push({
                fileName: relativePath,
                functionName: classMatch[1],
                exportType: 'class',
                line: lineNum
            });
            continue;
        }
        
        // export default function/class (可能需要多行匹配，这里简化处理单行)
        const defaultFuncMatch = lineContent.match(/export\s+default\s+(?:async\s+)?(?:function\s+(\w+)|class\s+(\w+))/);
        if (defaultFuncMatch) {
            const name = defaultFuncMatch[1] || defaultFuncMatch[2] || 'default';
            exports.push({
                fileName: relativePath,
                functionName: name,
                exportType: 'default',
                line: lineNum
            });
            continue;
        }
        
        // export { x, y, z } - 命名导出列表
        const namedListMatch = lineContent.match(/export\s*{([^}]+)}/);
        if (namedListMatch) {
            const items = namedListMatch[1].split(',').map(item => item.trim());
            for (const item of items) {
                // 匹配 "name" 或 "name as alias"
                const innerMatch = item.match(/^(\w+)(?:\s+as\s+(\w+))?$/);
                if (innerMatch) {
                    const name = innerMatch[1];
                    // 跳过内部变量如 event, config 等
                    if (name && !['event', 'config', 'state', 'props', 'children'].includes(name)) {
                        exports.push({
                            fileName: relativePath,
                            functionName: name,
                            exportType: 'named',
                            line: lineNum
                        });
                    }
                }
            }
        }
    }
    
    return exports;
}

// 在主进程中使用 grep 查找所有导出的引用
function findUsage(exportNames) {
    console.log(`正在搜索 ${exportNames.length} 个导出名的引用...`);
    
    const unusedExports = [];
    
    // 批量执行 grep 搜索
    for (const exp of exportNames) {
        try {
            // 在整个项目中搜索，除了自己的 export 声明行
            const result = execSync(
                `cd '${rootDir}' && grep -rn '\\b${exp.functionName}\\b' . --include='*.ts' --include='*.tsx' --include='*.js' --include='*.jsx' 2>/dev/null | grep -v '^[^:]*:${exp.line}:.*export' | head -20`,
                { encoding: 'utf-8' }
            );
            
            // 如果没有找到引用（或者只找到自己的文件），则为未使用
            if (!result || result.trim().length === 0) {
                unusedExports.push(exp);
            }
        } catch (e) {
            // grep 没有找到匹配项时返回非零退出码
            unusedExports.push(exp);
        }
    }
    
    return unusedExports;
}

// 主函数
function main() {
    console.log('开始扫描 TypeScript/JavaScript 文件...\n');
    
    // 获取所有符合条件的文件
    const files = getAllFiles(rootDir);
    console.log(`找到 ${files.length} 个文件待分析\n`);
    
    // 打印找到的文件（用于调试）
    for (const file of files) {
        console.log(`  [文件] ${path.relative(rootDir, file)}`);
    }
    console.log();
    
    // 分析每个文件
    for (const file of files) {
        const exports = extractExports(file);
        allExports.push(...exports);
    }
    
    console.log(`总共找到 ${allExports.length} 个导出\n`);
    
    // 去除重复
    const uniqueExports = [];
    const seen = new Set();
    
    for (const exp of allExports) {
        const key = `${exp.fileName}:${exp.functionName}`;
        if (!seen.has(key)) {
            seen.add(key);
            uniqueExports.push(exp);
        }
    }
    
    console.log(`去重后：${uniqueExports.length} 个导出\n`);
    
    // 查找未使用的导出
    console.log('开始查找未使用的导出...');
    const unused = findUsage(uniqueExports);
    
    console.log(`\n发现 ${unused.length} 个未使用的导出:\n`);
    
    // 输出 JSON 格式结果
    const result = unused.map(exp => ({
        fileName: exp.fileName,
        functionName: exp.functionName,
        exportType: exp.exportType,
        line: exp.line
    }));
    
    console.log('JSON 结果：');
    console.log(JSON.stringify(result, null, 2));
    
    // 保存为文件
    const outputPath = path.join(rootDir, 'unused_exports.json');
    fs.writeFileSync(outputPath, JSON.stringify(result, null, 2));
    console.log(`\n\n结果已保存到：${outputPath}`);
    
    return result;
}

// 运行主函数
main();
