// Run history navigation against the actual page, script and DOM in Chrome.
// No npm dependencies. CHROME_BIN can select an installed Chrome/Chromium.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
const {pathToFileURL} = require('node:url');
const root = path.resolve(__dirname, '..');
const candidates = [process.env.CHROME_BIN,
  process.platform === 'win32' && path.join(process.env.ProgramFiles || 'C:/Program Files', 'Google/Chrome/Application/chrome.exe'),
  '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].filter(Boolean);
const chrome = candidates.find(file => fs.existsSync(file));
if (!chrome) throw new Error('Chrome/Chromium required; set CHROME_BIN');

async function browserChecks() {
  const result = document.createElement('pre');
  result.id = 'regression-result';
  document.body.appendChild(result);
  let checks = 0;
  const assert = (ok, message) => { checks++; if (!ok) throw new Error(message); };
  const node = id => document.getElementById(id);
  const visible = id => !node(id).classList.contains('hide');
  const pixel = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=';
  const logs = [{id:'chat-1', prompt:'history prompt', reply:'**history reply**', model:'test-model', created_at:1},
    {id:'chat-2', prompt:'second prompt', reply:'second reply', created_at:2}];
  const jobs = [{job_id:'img-1', prompt:'history image', url:pixel, status:'completed', created_at:1},
    {job_id:'img-2', prompt:'second image', url:pixel, status:'completed', created_at:2}];
  let finishChat, finishImage;
  const reply = data => ({ok:true, text:async()=>JSON.stringify(data), json:async()=>data,
    headers:{get:()=> 'application/json'}});
  window.fetch = async url => {
    if (url === '/api/image-jobs') return reply({jobs});
    if (url === '/api/video-jobs') return reply({jobs:[]});
    if (url === '/api/chat-logs') return reply({logs});
    if (url === '/api/models') return reply({models:{text:['test-model'], image:['test-image'], video:['test-video']}});
    if (url === '/api/chat/v1/chat/completions') return new Promise(resolve => {finishChat = data => resolve(reply(data));});
    if (url === '/api/chat/v1/images/generations') return new Promise(resolve => {finishImage = data => resolve(reply(data));});
    return reply({});
  };
  try {
    state.admin = false;
    renderApp();
    await loadModels();
    await loadHistory();
    node('tabImage').click();
    const prompt = node('imgPrompt'), model = node('imgModel'), button = node('btnGenImg');
    prompt.value = 'my image draft';
    model.value = 'agnes-auto';
    node('imgRatio').value = '16:9';
    node('imgStyle').value = 'anime';
    node('imgResult').innerHTML = '<span>previous image result</span>';
    const generationHandler = button.onclick;
    for (let i=0; i<3; i++) {
      node('imgHistory').querySelector('.history-item').click();
      assert(visible('historyView') && !visible('imageView'), 'image history visibility');
      await showImgJob('img-2');
      assert(node('historyView').textContent.includes('img-2'), 'replace history detail');
      node('btnHistoryBack').click();
      assert(visible('imageView') && !visible('historyView'), 'return to image form');
      assert(node('imgPrompt') === prompt && prompt.value === 'my image draft', 'preserve image prompt node/draft');
      assert(node('imgModel') === model && model.value === 'agnes-auto', 'preserve model');
      assert(node('imgRatio').value === '16:9' && node('imgStyle').value === 'anime', 'preserve image settings');
      assert(node('imgResult').textContent === 'previous image result', 'preserve generated result');
      assert(button.onclick === generationHandler, 'preserve generation event');
    }
    await showImgJob('img-1');
    node('tabVideo').click();
    assert(visible('videoView') && !visible('historyView'), 'leave history for video');
    node('tabImage').click();
    assert(visible('imageView') && node('imgPrompt') === prompt, 'tab returns to form');
    await showImgJob('missing');
    assert(visible('imageView'), 'missing history leaves form usable');

    node('tabChat').click();
    const liveMessage = addMsg('ass','live reply','test-model',1);
    node('textInput').value = 'chat draft';
    const chatModel = node('chatModel');
    node('historyList').querySelector('.history-item').click();
    assert(visible('historyView') && !visible('chatView') && !visible('inputArea'), 'chat detail hides live composer');
    assert(node('historyView').querySelector('strong').textContent === 'history reply', 'render history markdown');
    showChatLog('chat-2');
    assert(node('historyView').textContent.includes('second reply') && !node('historyView').textContent.includes('history reply'), 'switch chat details');
    node('btnHistoryBack').click();
    assert(visible('chatView') && visible('inputArea') && !visible('historyView'), 'chat return button');
    assert(node('chatView').firstElementChild === liveMessage, 'preserve live conversation');
    assert(node('textInput').value === 'chat draft' && node('chatModel') === chatModel, 'preserve chat draft/model');

    // Complete real requests while their live views are hidden by history.
    const pendingChat = sendChat();
    showChatLog('chat-1');
    finishChat({choices:[{message:{content:'completed live reply'}}]});
    await pendingChat;
    assert(!node('historyView').textContent.includes('completed live reply'), 'live reply does not overwrite history');
    node('btnHistoryBack').click();
    assert(node('chatView').textContent.includes('completed live reply') && !node('btnSend').disabled, 'return to completed live reply');
    node('textInput').value = 'next draft';
    showChatLog('chat-1');
    node('tabImage').click();
    const pendingImage = generateImage();
    await showImgJob('img-1');
    finishImage({data:[{url:pixel}]});
    await pendingImage;
    node('btnHistoryBack').click();
    assert(node('imgResult').querySelector('img') !== null && node('imgPrompt') === prompt, 'image completes during history inspection');
    node('tabChat').click();
    assert(visible('inputArea') && node('textInput').value === 'next draft', 'cross-tab restores chat composer');
    result.dataset.status = 'passed';
    result.textContent = checks + ' browser history navigation assertions passed';
  } catch (error) {
    result.dataset.status = 'failed';
    result.textContent = error.stack;
  }
}

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'agens-chat-test-'));
try {
  const source = fs.readFileSync(path.join(root,'internal/web/static/chat.main.js'),'utf8');
  const html = fs.readFileSync(path.join(root,'internal/web/static/chat.html'),'utf8').replace(
    '<script src="chat.main.js"></script>',
    () => '<script>' + source.replace(/<\/script/gi,'<\\/script') +
      '\ncheckChatAuth=()=>{};\nwindow.addEventListener("DOMContentLoaded",' + browserChecks.toString() + ');</script>');
  const fixture = path.join(tmp,'fixture.html');
  fs.writeFileSync(fixture,html);
  const run = spawnSync(chrome,['--headless','--no-sandbox','--disable-gpu','--no-first-run',
    '--disable-background-networking','--user-data-dir='+path.join(tmp,'profile'),
    '--dump-dom','--virtual-time-budget=5000',pathToFileURL(fixture).href],
    {encoding:'utf8',timeout:45000,windowsHide:true,maxBuffer:8*1024*1024});
  if (run.error) throw run.error;
  const match = /<pre id="regression-result" data-status="([^"]+)">([\s\S]*?)<\/pre>/.exec(run.stdout);
  if (run.status !== 0 || !match || match[1] !== 'passed') {
    throw new Error(match ? match[2] : 'Chrome did not finish navigation tests: '+run.stderr.slice(-2000));
  }
  console.log(match[2]);
} finally {
  fs.rmSync(tmp,{recursive:true,force:true});
}
