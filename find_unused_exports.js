const fs = require('fs');
const path = require('path');
const { execSync } = require('child_process');

const rootDir = '/home/zhangdailin/Documents/Orchids-2api';

// UI 组件库黑名单
const UI_COMPONENTS = [
    'button', 'input', 'dialog', 'sheet', 'tabs', 'table', 'select', 
    'dropdown-menu', 'popover', 'tooltip', 'calendar', 'switch', 
    'badge', 'spinner', 'message', 'checkbox', 'label', 'textarea'
];

function shouldExclude(filePath) {
    const relativePath = path.relative(rootDir, filePath);
    
    if (relativePath.includes('node_modules')) return true;
    if (/\.test\.(ts|tsx|js)$/.test(relativePath)) return true;
    if (/\.spec\.(ts|tsx|js)$/.test(relativePath)) return true;
    if (relativePath.includes('.audit/') || relativePath.includes('.upstream/')) return true;
    if (relativePath.includes('.git/') || relativePath.includes('.npm/') || relativePath.includes('venv/')) return true;
    
    // 排除 UI 组件
    for (const ui of UI_COMPONENTS) {
        if (relativePath.includes(`/${ui}/`) || relativePath.includes(`/${ui}.`)) return true;
    }
    
    return false;
}

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
    } catch (e) {}
    return results;
}

function extractExports(filePath) {
    const content = fs.readFileSync(filePath, 'utf-8');
    const lines = content.split('\n');
    const relativePath = path.relative(rootDir, filePath);
    const exports = [];
    
    for (let i = 0; i < lines.length; i++) {
        const lineNum = i + 1;
        const lineContent = lines[i];
        
        // export function xxx
        const funcMatch = lineContent.match(/export\s+(?:async\s+)?function\s+(\w+)/);
        if (funcMatch) {
            exports.push({ fileName: relativePath, functionName: funcMatch[1], exportType: 'named', line: lineNum });
            continue;
        }
        
        // export const/let/var xxx
        const varMatch = lineContent.match(/export\s+(const|let|var)\s+(\w+)/);
        if (varMatch) {
            exports.push({ fileName: relativePath, functionName: varMatch[2], exportType: 'named', line: lineNum });
            continue;
        }
        
        // export class xxx
        const classMatch = lineContent.match(/export\s+class\s+(\w+)/);
        if (classMatch) {
            exports.push({ fileName: relativePath, functionName: classMatch[1], exportType: 'class', line: lineNum });
            continue;
        }
        
        // export default
        const defaultMatch = lineContent.match(/export\s+default\s+/);
        if (defaultMatch) {
            exports.push({ fileName: relativePath, functionName: 'default', exportType: 'default', line: lineNum });
            continue;
        }
        
        // export { x, y, z }
        const namedListMatch = lineContent.match(/export\s*{([^}]+)}/);
        if (namedListMatch) {
            const items = namedListMatch[1].split(',').map(item => item.trim());
            for (const item of items) {
                const innerMatch = item.match(/^(\w+)(?:\s+as\s+(\w+))?$/);
                if (innerMatch) {
                    const name = innerMatch[1];
                    if (name && !['event', 'config', 'state', 'props', 'children'].includes(name)) {
                        exports.push({ fileName: relativePath, functionName: name, exportType: 'named', line: lineNum });
                    }
                }
            }
        }
    }
    
    return exports;
}

function findUsage(exportNames) {
    console.log(`搜索 ${exportNames.length} 个导出名的引用...`);
    const unusedExports = [];
    
    for (const exp of exportNames) {
        try {
            const result = execSync(
                `cd '${rootDir}' && grep -rn '\\b${exp.functionName}\\b' . --include='*.ts' --include='*.tsx' --include='*.js' --include='*.jsx' 2>/dev/null | grep -v '^[^:]*:${exp.line}:.*export' | head -20`,
                { encoding: 'utf-8' }
            );
            
            if (!result || result.trim().length === 0) {
                unusedExports.push(exp);
            }
        } catch (e) {
            unusedExports.push(exp);
        }
    }
    
    return unusedExports;
}

function main() {
    console.log('开始扫描...\n');
    
    const files = getAllFiles(rootDir);
    console.log(`找到 ${files.length} 个文件\n`);
    
    const allExports = [];
    for (const file of files) {
        const exports = extractExports(file);
        allExports.push(...exports);
    }
    
    console.log(`总共找到 ${allExports.length} 个导出\n`);
    
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
    
    console.log('查找未使用的导出...');
    const unused = findUsage(uniqueExports);
    
    console.log(`\n发现 ${unused.length} 个未使用的导出:\n`);
    
    const result = unused.map(exp => ({
        fileName: exp.fileName,
        functionName: exp.functionName,
        exportType: exp.exportType,
        line: exp.line
    }));
    
    console.log(JSON.stringify(result, null, 2));
    
    const outputPath = path.join(rootDir, 'unused_exports.json');
    fs.writeFileSync(outputPath, JSON.stringify(result, null, 2));
    console.log(`\n结果已保存到：${outputPath}`);
    
    return result;
}

main();
