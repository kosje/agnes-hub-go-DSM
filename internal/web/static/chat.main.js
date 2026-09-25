// chat.main.js - Agnes Chat UI logic

// 工具函数（与 chat.html 内联脚本等价，避免重复声明）
const API_URL = "";
async function api(p, o = {}) {
  const opts = { credentials: "same-origin", headers: { "Content-Type": "application/json" }, ...o };
  const r = await fetch(API_URL + p, opts);
  const t = await r.text();
  let d = null; try { d = t ? JSON.parse(t) : null; } catch (e) { d = { raw: t }; }
  if (!r.ok) { const m = (d && d.error && d.error.message) || ("HTTP " + r.status); throw new Error(m); }
  return d;
}
function esc(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
function toast(m, k) { const e = document.getElementById("toast"); if (!e) return; e.className = "banner " + (k || "good"); e.textContent = m; e.classList.remove("hide"); clearTimeout(e._t); e._t = setTimeout(() => e.classList.add("hide"), 4000); }
function fmtDate(ts) { if (!ts) return ""; const d = new Date(ts * 1000); return d.toLocaleDateString("zh-CN") + " " + d.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" }); }
function timeAgo(ts) { const s = Math.floor(Date.now() / 1000) - ts; if (s < 60) return s + "秒前"; if (s < 3600) return Math.floor(s / 60) + "分钟前"; if (s < 86400) return Math.floor(s / 3600) + "小时前"; return Math.floor(s / 86400) + "天前"; }

const state = { tab: "chat", session: null, keys: [], accounts: [], activeKey: null, chatLogs: [], curHistoryId: null, messages: [], model: "agnes-3.0-flash", sending: false, imJobId: null };

/* ==================== LOGIN ==================== */
async function checkSession(){
  try{
    const s=await api("/api/session");
    if(s&&s.logged_in){
      state.session=s;
      renderApp();
      return;
    }
  }catch(e){}
  renderLogin();
}

async function checkChatAuth(){
  try{
    const s=await api("/api/chat/session");
    if(s.authenticated){
      // 已验证或无需密码，检查管理员会话
      try{
        const admin=await api("/api/session");
        if(admin.logged_in){
          state.session=admin;
          renderApp();
          return;
        }
      }catch(e){}
      // 无管理员会话，显示管理员登录
      state.chatAuth=true;
      renderAdminLogin();
      return;
    }
    if(s.requires_password){
      // 需要 chat 密码
      state.requiresChatPassword=true;
      renderChatPasswordLogin();
      return;
    }
  }catch(e){}
  renderLogin();
}

function renderChatPasswordLogin(){
  document.getElementById("app").innerHTML=`
  <div class="login">
    <div class="card">
      <h1>Agnes AI 助手</h1>
      <p class="muted">AI 对话 · 生图 · 生视频</p>
      <div id="toast" class="hide"></div>
      <label>Chat 访问密码</label>
      <input id="chatPw" type="password" placeholder="请输入 Chat 访问密码">
      <div style="margin-top:12px"><button class="primary" id="btnChatLogin">登录</button></div>
      <div class="hint">请输入管理员设置的 Chat 访问密码</div>
    </div>
  </div>`;
  document.getElementById("btnChatLogin").onclick=async()=>{
    try{
      await api("/api/chat/login",{method:"POST",body:JSON.stringify({password:document.getElementById("chatPw").value})});
      state.chatAuth=true;
      renderApp();
    }catch(e){toast(e.message,"bad");}
  };
  document.getElementById("chatPw").onkeydown=e=>{if(e.key==="Enter")document.getElementById("btnChatLogin").click();};
  document.getElementById("chatPw").focus();
}

function renderAdminLogin(){
  document.getElementById("app").innerHTML=`
  <div class="login">
    <div class="card">
      <h1>Agnes AI 助手</h1>
      <p class="muted">AI 对话 · 生图 · 生视频</p>
      <div id="toast" class="hide"></div>
      <label>管理员密码</label>
      <input id="adminPw" type="password" placeholder="请输入管理员密码">
      <div style="margin-top:12px"><button class="primary" id="btnAdminLogin">登录</button></div>
      <div class="hint">请输入安装时设置的管理员密码</div>
    </div>
  </div>`;
  document.getElementById("btnAdminLogin").onclick=async()=>{
    try{
      await api("/api/login",{method:"POST",body:JSON.stringify({password:document.getElementById("adminPw").value})});
      const admin=await api("/api/session");
      state.session=admin;
      renderApp();
    }catch(e){toast(e.message,"bad");}
  };
  document.getElementById("adminPw").onkeydown=e=>{if(e.key==="Enter")document.getElementById("btnAdminLogin").click();};
  document.getElementById("adminPw").focus();
}

function renderLogin(){
  document.getElementById("app").innerHTML=`
  <div class="login">
    <div class="card">
      <h1>Agnes AI 助手</h1>
      <p class="muted">AI 对话 · 生图 · 生视频</p>
      <div id="toast" class="hide"></div>
      <label>访问密码</label>
      <input id="pw" type="password" placeholder="请输入访问密码">
      <div style="margin-top:12px"><button class="primary" id="btnLogin">登录</button></div>
      <div class="hint">访问密码由控制台设置；未设置则直接进入</div>
    </div>
  </div>`;
  document.getElementById("btnLogin").onclick=async()=>{
    try{
      await api("/api/login",{method:"POST",body:JSON.stringify({password:document.getElementById("pw").value})});
      state.session={logged_in:true};
      renderApp();
    }catch(e){toast(e.message,"bad");}
  };
  document.getElementById("pw").onkeydown=e=>{if(e.key==="Enter")document.getElementById("btnLogin").click();};
  document.getElementById("pw").focus();
}

/* ==================== APP SHELL ==================== */
function renderApp(){
  document.getElementById("app").innerHTML=`
  <div class="topbar">
    <h1><img class="logo" src="/logo.png" alt="Agnes">Agnes AI 助手</h1>
    <span id="topbarInfo" class="topbar-info">加载中...</span>
    <button class="sm" id="btnRefresh">刷新</button>
    <button class="sm danger" id="btnLogout">退出</button>
  </div>
  <div class="chat-layout">
    <div class="chat-main">
      <div class="tabs">
        <button data-tab="chat" class="on" id="tabChat">💬 对话</button>
        <button data-tab="image" id="tabImage">🎨 生图</button>
        <button data-tab="video" id="tabVideo">🎬 生视频</button>
      </div>
      <div id="chatView" class="msg-list"></div>
      <div id="imageView" class="hide" style="padding:16px;overflow-y:auto;flex:1"></div>
      <div id="videoView" class="hide" style="padding:16px;overflow-y:auto;flex:1"></div>
      <div id="inputArea" class="input-area">
        <div class="setting-row" style="margin-bottom:8px">
          <div class="setting-group" style="max-width:260px">
            <label>对话模型</label>
            <select id="chatModel">
              <option value="agnes-2.5-flash" selected>agnes-2.5-flash</option>
              <option value="agnes-2.0-flash">agnes-2.0-flash</option>
              <option value="agnes-auto">agnes-auto（自动）</option>
            </select>
          </div>
        </div>
        <div class="input-row">
          <input id="textInput" class="msg-input" placeholder="输入消息... (Enter 发送，Shift+Enter 换行)" rows="3">
          <button class="primary send-btn" id="btnSend">发送</button>
        </div>
      </div>
    </div>
    <div class="sidebar">
      <h3>📋 历史记录</h3>
      <div id="historyList"></div>
      <div id="imgHistory" class="hide"></div>
      <div id="vidHistory" class="hide"></div>
    </div>
  </div>
  <div id="toast" class="hide"></div>`;

  // Load accounts (auto-pool, no manual key selection)
  Promise.all([api("/api/keys"), api("/api/accounts")]).then(([keysRes, accountsRes])=>{
    state.keys=(keysRes.keys||[]).filter(k=>k.enabled);
    state.accounts=accountsRes.accounts||[];
    const info=document.getElementById("topbarInfo");
    if(state.accounts.length>0){
      const names=state.accounts.map(a=>esc(a.name)).join("、");
      info.textContent=`账号池: ${state.accounts.length} 个 · ${names}`;
    }else if(state.keys.length>0){
      info.textContent=`已配置 ${state.keys.length} 个密钥`;
    }else{
      info.textContent="未配置账号";
      toast("未配置任何可用的 API Key 或账号","warn");
    }
  }).catch(()=>{});

  document.getElementById("tabChat").onclick=()=>switchTab("chat");
  document.getElementById("tabImage").onclick=()=>switchTab("image");
  document.getElementById("tabVideo").onclick=()=>switchTab("video");
  document.getElementById("btnRefresh").onclick=loadHistory;
  document.getElementById("btnLogout").onclick=async()=>{
    await api("/api/logout",{method:"POST"});
    location.reload();
  };
  // 移动端侧边栏开关
  const hamburger=document.getElementById("btnHamburger");
  const sidebar=document.getElementById("sidebar");
  const overlay=document.getElementById("sidebarOverlay");
  if(hamburger){
    const toggleSidebar=()=>{const open=sidebar.classList.toggle("open");overlay.classList.toggle("show",open);};
    hamburger.onclick=toggleSidebar;
    overlay.onclick=toggleSidebar;
    // 窗口变大时自动关闭侧边栏
    window.addEventListener("resize",()=>{if(window.innerWidth>768){sidebar.classList.remove("open");overlay.classList.remove("show");}});
  }

  document.getElementById("btnSend").onclick=sendChat;
  document.getElementById("textInput").onkeydown=e=>{if(e.key==="Enter"&&!e.shiftKey){e.preventDefault();sendChat();}};
  const chatModelSel=document.getElementById("chatModel");
  if(chatModelSel){ chatModelSel.value=state.model; chatModelSel.onchange=e=>{state.model=e.target.value;}; }

  // 渲染生图/生视频视图（必须在 renderApp 之后调用）
  renderImageView();
  renderVideoView();

  loadHistory();
  loadModels();
}

function switchTab(tab){
  state.tab=tab;
  document.querySelectorAll(".tabs button").forEach(b=>b.classList.toggle("on",b.dataset.tab===tab));
  document.getElementById("chatView").classList.toggle("hide",tab!=="chat");
  document.getElementById("imageView").classList.toggle("hide",tab!=="image");
  document.getElementById("videoView").classList.toggle("hide",tab!=="video");
  document.getElementById("inputArea").classList.toggle("hide",tab!=="chat");
  document.getElementById("historyList").classList.toggle("hide",tab!=="chat");
  document.getElementById("imgHistory").classList.toggle("hide",tab!=="image");
  document.getElementById("vidHistory").classList.toggle("hide",tab!=="video");
}

/* ==================== CHAT ==================== */
function addMsg(role,content,model,ts){
  const div=document.createElement("div");
  div.className="msg "+(role==="user"?"user":"ass");
  const meta=model?`<div class="msg-meta">${esc(model)} · ${fmtDate(ts)}</div>`:"";
  div.innerHTML=`<div class="msg-bubble">${role==="user"?esc(content):renderMarkdown(content)}</div>${meta}`;
  const list=document.getElementById("chatView");
  list.appendChild(div);
  list.scrollTop=list.scrollHeight;
  return div;
}

async function sendChat(){
  if(state.sending)return;
  const input=document.getElementById("textInput");
  const text=input.value.trim();
  if(!text)return;
  input.value="";
  state.sending=true;
  document.getElementById("btnSend").disabled=true;
  document.getElementById("btnSend").innerHTML='<span class="spinner"></span> 思考中...';

  // Add user message
  const userMsg=addMsg("user",text,null,Date.now()/1000);

  // Add assistant placeholder
  const assMsg=addMsg("ass","",state.model||"agnes-auto",Date.now()/1000);
  const bubble=assMsg.querySelector(".msg-bubble");
  bubble.innerHTML='<span class="spinner"></span> 正在生成回答...';

  try{
    // 注意：服务端返回 SSE 流式（text/event-stream）；需自行解析 data: 帧，
    // api() 的 JSON.parse 无法处理 SSE，故这里用原生 fetch 流式读取。
    const resp=await fetch(API_URL+"/api/chat/v1/chat/completions",{
      method:"POST",
      credentials:"same-origin",
      headers:{"Content-Type":"application/json"},
      body:JSON.stringify({
        model:state.model||"agnes-auto",
        messages:[{role:"user",content:text}],
        stream:true
      })
    });
    if(!resp.ok){
      let msg="HTTP "+resp.status;
      try{ const j=await resp.json(); if(j&&j.error&&j.error.message)msg=j.error.message; }catch(e){}
      throw new Error(msg);
    }
    const ct=resp.headers.get("content-type")||"";
    bubble.textContent="";
    let full="";
    if(ct.indexOf("text/event-stream")>=0){
      const reader=resp.body.getReader();
      const dec=new TextDecoder();
      let buf="";
      while(true){
        const {done,value}=await reader.read();
        if(done)break;
        buf+=dec.decode(value,{stream:true});
        let idx;
        while((idx=buf.indexOf("\n"))>=0){
          const line=buf.slice(0,idx);
          buf=buf.slice(idx+1);
          const t=line.trim();
          if(!t.startsWith("data:"))continue;
          const data=t.slice(5).trim();
          if(data==="[DONE]")continue;
          try{
            const j=JSON.parse(data);
            const c=(j.choices&&j.choices[0])||{};
            const d=c.delta||{};
            if(d.content)full+=d.content;
            else if(c.message&&c.message.content)full=c.message.content;
            bubble.innerHTML=renderMarkdown(full);
          }catch(e){}
        }
      }
    }else{
      // 非流式 JSON 兜底
      const j=await resp.json();
      const c=(j.choices&&j.choices[0])||{};
      full=(c.message&&c.message.content)||(c.delta&&c.delta.content)||c.text||"";
      bubble.innerHTML=renderMarkdown(full);
    }
    if(!full)bubble.textContent="（无内容返回）";
    // 持久化到服务端聊天记录
    try{
      await api("/api/chat-logs",{method:"POST",body:JSON.stringify({model:state.model||"agnes-auto",prompt:text,reply:stripInlineImages(full),status:"completed"})});
      await loadHistory();
    }catch(e){ console.error("save chat log failed", e); }
  }catch(e){
    bubble.textContent="错误: "+e.message;
    bubble.style.color="var(--bad)";
  }finally{
    state.sending=false;
    document.getElementById("btnSend").disabled=false;
    document.getElementById("btnSend").textContent="发送";
  }
}

/* ==================== IMAGE GENERATION ==================== */
function renderImageView(){
  const view=document.getElementById("imageView");
  view.innerHTML=`
    <div class="card">
      <h3 style="margin:0 0 12px;font-size:15px">🎨 AI 生图</h3>
      <label>提示词</label>
      <textarea id="imgPrompt" placeholder="描述你想生成的图片... 例如：一只可爱的猫咪在夕阳下奔跑"></textarea>
      
      <div class="img-settings" style="margin-top:14px">
        <h4>⚙️ 生成设置</h4>
        <div class="setting-row">
          <div class="setting-group">
            <label>图片比例</label>
            <select id="imgRatio">
              <option value="1:1">1:1 方形 (1024×1024)</option>
              <option value="16:9">16:9 宽屏 (1280×720)</option>
              <option value="9:16">9:16 竖屏 (720×1280)</option>
              <option value="4:3">4:3 标准 (1024×768)</option>
              <option value="3:4">3:4 肖像 (768×1024)</option>
            </select>
          </div>
          <div class="setting-group">
            <label>艺术风格</label>
            <select id="imgStyle">
              <option value="">默认 (无特殊风格)</option>
              <option value="photorealistic">写实摄影</option>
              <option value="anime">动漫风格</option>
              <option value="oil-painting">油画风格</option>
              <option value="watercolor">水彩风格</option>
              <option value="pixel-art">像素艺术</option>
              <option value="3d-render">3D 渲染</option>
              <option value="sketch">素描手绘</option>
            </select>
          </div>
        </div>
        <div class="setting-row">
          <div class="setting-group">
            <label>生成模型</label>
            <select id="imgModel">
              <option value="agnes-image-2.5-flash" selected>agnes-image-2.5-flash</option>
              <option value="agnes-auto">agnes-auto (自动选择)</option>
              <option value="agnes-image-2.1-flash">agnes-image-2.1-flash</option>
              <option value="dall-e-3">dall-e-3</option>
            </select>
          </div>
        </div>
      </div>
      
      <div style="margin-top:12px">
        <button class="primary" id="btnGenImg" style="width:100%">🖼️ 开始生成</button>
      </div>
      <div id="imgResult" style="margin-top:16px"></div>
    </div>`;
  document.getElementById("btnGenImg").onclick=generateImage;
}

async function generateImage(){
  const prompt=document.getElementById("imgPrompt").value.trim();
  const model=document.getElementById("imgModel").value;
  if(!prompt)return;
  const result=document.getElementById("imgResult");
  result.innerHTML='<div class="muted"><span class="spinner"></span> 正在生成图片...</div>';
  try{
    const ratio=document.getElementById("imgRatio").value;
    const style=document.getElementById("imgStyle").value;
    // Map ratio to size string
    const ratioToSize={
      "1:1":"1024x1024","16:9":"1280x720","9:16":"720x1280",
      "4:3":"1024x768","3:4":"768x1024"
    };
    const body={model,prompt,n:1,size:ratioToSize[ratio]||"1024x1024"};
    if(style) body.style=style;
    const resp=await api("/api/chat/v1/images/generations",{
      method:"POST",
      body:JSON.stringify(body)
    });
    result.innerHTML="";
    const data=resp.data||[];
    data.forEach(item=>{
      const url=item.url||item.b64_json;
      const div=document.createElement("div");
      if(item.url){
        div.innerHTML=`<img src="${esc(item.url)}" alt="${esc(prompt)}" class="gen-image" onclick="window.open(this.src)">`;
      }else if(item.b64_json){
        div.innerHTML=`<img src="data:image/png;base64,${esc(item.b64_json)}" alt="${esc(prompt)}" class="gen-image">`;
      }
      result.appendChild(div);
    });
  }catch(e){
    result.innerHTML=`<div class="banner bad">${esc(e.message)}</div>`;
  }
}

/* ==================== VIDEO GENERATION ==================== */
function renderVideoView(){
  const view=document.getElementById("videoView");
  view.innerHTML=`
    <div class="card">
      <h3 style="margin:0 0 12px;font-size:15px">🎬 AI 生视频</h3>
      <label>提示词</label>
      <textarea id="vidPrompt" placeholder="描述你想生成的视频... 例如：夕阳下的海滩，海浪轻拍沙滩"></textarea>
      
      <div style="margin-top:12px;display:flex;gap:8px;align-items:center">
        <select id="vidModel" style="width:200px">
          <option value="agnes-video-2.5-flash" selected>agnes-video-2.5-flash</option>
          <option value="agnes-auto">agnes-auto (自动选择)</option>
          <option value="agnes-video-v2.0">agnes-video-v2.0</option>
        </select>
        <button class="primary" id="btnGenVid">🎥 提交生成</button>
      </div>
      <div id="vidResult" style="margin-top:16px"></div>
    </div>`;
  document.getElementById("btnGenVid").onclick=generateVideo;
}

async function generateVideo(){
  const prompt=document.getElementById("vidPrompt").value.trim();
  const model=document.getElementById("vidModel").value;
  if(!prompt)return;
  const result=document.getElementById("vidResult");
  result.innerHTML='<div class="muted"><span class="spinner"></span> Submitting...</div>';
  try{
    const resp=await api("/api/chat/v1/videos",{
      method:"POST",
      body:JSON.stringify({model,prompt})
    });
    const jobId=resp.job_id||resp.id;
    if(jobId){
      result.innerHTML=`<div class="banner good">✅ 视频已提交生成！任务 ID: ${esc(jobId)}</div>`;
      pollVideo(jobId,result);
    }
  }catch(e){
    result.innerHTML=`<div class="banner bad">❌ ${esc(e.message)}</div>`;
  }
}

async function pollVideo(jobId,resultEl){
  const poll=async()=>{
    try{
      const resp=await api(`/api/chat/v1/videos/${jobId}`);
      if(resp.status==="completed"&&resp.video_url){
        resultEl.innerHTML+=`<div class="video-wrap"><video controls src="${esc(resp.video_url)}"></video></div>`;
        return;
      }else if(resp.status==="failed"){
        resultEl.innerHTML+=`<div class="banner bad">❌ 生成失败: ${esc(resp.error||"未知错误")}</div>`;
        return;
      }
      setTimeout(poll,3000);
    }catch(e){
      resultEl.innerHTML+=`<div class="banner warn">⚠️ 轮询错误: ${esc(e.message)}</div>`;
    }
  };
  setTimeout(poll,3000);
}

/* ==================== HISTORY ==================== */
async function loadHistory(){
  try{
    const [imgJobs,vidJobs,chatLogs]=await Promise.all([
      api("/api/image-jobs"),
      api("/api/video-jobs"),
      api("/api/chat-logs")
    ]);
    state.imgJobs=imgJobs.jobs||[];
    state.vidJobs=vidJobs.jobs||[];
    state.chatLogs=chatLogs.logs||[];
    renderHistory();
  }catch(e){console.error(e);}
}

function renderHistory(){
  // 聊天对话记录
  const chatList=document.getElementById("historyList");
  if(chatList&&state.chatLogs){
    if(state.chatLogs.length===0){
      chatList.innerHTML='<div class="muted" style="font-size:12px;padding:4px 2px">暂无对话记录</div>';
    }else{
      const items=state.chatLogs.slice(0,50).map(j=>`
        <div class="history-item" onclick="showChatLog('${esc(j.id)}')">
          <button class="del" title="删除" onclick="event.stopPropagation();deleteChatLog('${esc(j.id)}')">×</button>
          <div class="muted">${timeAgo(j.created_at)} · ${esc((j.model||"").substring(0,18))}</div>
          <div>${esc((j.prompt||"").substring(0,40))}</div>
        </div>`).join("");
      chatList.innerHTML=`<div class="hist-head"><span>对话记录 (${state.chatLogs.length})</span><button class="sm" onclick="clearChatLogs()">清空</button></div>`+items;
    }
  }
  const imgList=document.getElementById("imgHistory");
  const vidList=document.getElementById("vidHistory");
  if(imgList&&state.imgJobs){
    const items=(state.imgJobs.slice(-20).reverse()).map(j=>`
      <div class="history-item" onclick="showImgJob('${esc(j.job_id)}')">
        <button class="del" title="删除" onclick="event.stopPropagation();deleteImgJob('${esc(j.job_id)}')">×</button>
        <div class="muted">${timeAgo(j.created_at)}</div>
        <div>${esc((j.prompt||"").substring(0,40))}</div>
        <div class="tag ok">${esc(j.status)}</div>
      </div>`).join("");
    imgList.innerHTML=`<div class="hist-head"><span>图片记录 (${state.imgJobs.length})</span><button class="sm" onclick="clearImgJobs()">清空</button></div>`+items;
  }
  if(vidList&&state.vidJobs){
    const items=(state.vidJobs.slice(-20).reverse()).map(j=>`
      <div class="history-item">
        <button class="del" title="删除" onclick="event.stopPropagation();deleteVidJob('${esc(j.job_id)}')">×</button>
        <div class="muted">${timeAgo(j.created_at)}</div>
        <div>${esc((j.prompt||"").substring(0,40))}</div>
        <div class="tag ${j.status==='completed'?'ok':j.status==='failed'?'warn':''}">${esc(j.status)}</div>
      </div>`).join("");
    vidList.innerHTML=`<div class="hist-head"><span>视频记录 (${state.vidJobs.length})</span><button class="sm" onclick="clearVidJobs()">清空</button></div>`+items;
  }
}

async function deleteChatLog(id){
  try{ await api("/api/chat-logs/"+encodeURIComponent(id),{method:"DELETE"}); state.chatLogs=state.chatLogs.filter(j=>j.id!==id); renderHistory(); toast("已删除对话记录","good"); }
  catch(e){ toast(e.message,"bad"); }
}
async function clearChatLogs(){
  if(!confirm("确认清空全部对话记录？此操作不可恢复。"))return;
  try{ await api("/api/chat-logs/clear",{method:"POST"}); state.chatLogs=[]; renderHistory(); toast("已清空对话记录","good"); }
  catch(e){ toast(e.message,"bad"); }
}
function showChatLog(id){
  const j=state.chatLogs.find(x=>x.id===id);
  if(!j)return;
  switchTab("chat");
  const view=document.getElementById("chatView");
  view.innerHTML="";
  addMsg("user",j.prompt,null,j.created_at||Date.now()/1000);
  addMsg("ass",j.reply||"",j.model,j.created_at||Date.now()/1000);
  view.scrollTop=view.scrollHeight;
}

async function deleteImgJob(id){
  try{ await api("/api/image-jobs/"+encodeURIComponent(id),{method:"DELETE"}); state.imgJobs=state.imgJobs.filter(j=>j.job_id!==id); renderHistory(); toast("已删除图片记录","good"); }
  catch(e){ toast(e.message,"bad"); }
}
async function clearImgJobs(){
  if(!confirm("确认清空全部图片记录？此操作不可恢复。"))return;
  try{ await api("/api/image-jobs/clear",{method:"POST"}); state.imgJobs=[]; renderHistory(); toast("已清空图片记录","good"); }
  catch(e){ toast(e.message,"bad"); }
}
async function deleteVidJob(id){
  try{ await api("/api/video-jobs/"+encodeURIComponent(id),{method:"DELETE"}); state.vidJobs=state.vidJobs.filter(j=>j.job_id!==id); renderHistory(); toast("已删除视频记录","good"); }
  catch(e){ toast(e.message,"bad"); }
}
async function clearVidJobs(){
  if(!confirm("确认清空全部视频记录？此操作不可恢复。"))return;
  try{ await api("/api/video-jobs/clear",{method:"POST"}); state.vidJobs=[]; renderHistory(); toast("已清空视频记录","good"); }
  catch(e){ toast(e.message,"bad"); }
}

async function showImgJob(jobId){
  try{
    const resp=await api(`/api/image-jobs/${jobId}`);
    const view=document.getElementById("imageView");
    view.innerHTML=`
      <button class="sm" onclick="switchTab('image')">← 返回</button>
      <h3>图片任务: ${esc(jobId)}</h3>
      <p class="muted">模型: ${esc(resp.model||"?")} · 状态: ${esc(resp.status||"?")}</p>
      <img src="${esc(resp.url||"")}" style="max-width:100%;border-radius:8px;margin-top:10px">`;
  }catch(e){toast(e.message,"bad");}
}

/* ==================== MODELS ==================== */
async function loadModels(){
  try{
    const resp=await api("/api/models");
    state.models=(resp&&resp.models)||null;
    populateModelSelects();
  }catch(e){ console.error("loadModels failed", e); }
}

function populateModelSelects(){
  if(!state.models)return;
  const defaults={chat:"agnes-3.0-flash", image:"agnes-image-2.5-flash", video:"agnes-video-2.5-flash"};
  const cfg=[
    {id:"chatModel", mod:"text", def:defaults.chat},
    {id:"imgModel", mod:"image", def:defaults.image},
    {id:"vidModel", mod:"video", def:defaults.video}
  ];
  cfg.forEach(({id,mod,def})=>{
    const sel=document.getElementById(id);
    if(!sel)return;
    const list=state.models[mod]||[];
    if(list.length===0)return;
    // 文字模型优先采用用户/默认偏好（agnes-3.0-flash），图/视频沿用各自静态默认值
    const current = (mod==="text")
      ? ((state.model && list.indexOf(state.model)>=0) ? state.model : def)
      : (sel.value||def);
    let opts=`<option value="agnes-auto">agnes-auto（自动选择）</option>`;
    list.forEach(m=>{
      const selected=m===current?" selected":"";
      opts+=`<option value="${esc(m)}"${selected}>${esc(m)}</option>`;
    });
    sel.innerHTML=opts;
    if(list.indexOf(current)>=0){
      sel.value=current;
    }else if(list.indexOf(def)>=0){
      sel.value=def;
    }else{
      sel.value="agnes-auto";
    }
  });
  const chatSel=document.getElementById("chatModel");
  if(chatSel) state.model=chatSel.value;
}

/* ==================== UTILS ==================== */
// stripInlineImages 把正文里的 base64 图片压成一行提示。
//
// 服务端为了让图片能在浏览器里显示，会把生成结果回取并内联成
// data:image/...;base64,... —— 单张图动辄 1~3 MB，转成 base64 还要再涨三分之一。
// 这段正文如果原样 POST 到 /api/chat-logs，服务端的 JSON 存储会被瞬间撑爆
// （几十条记录就能上百 MB），而对话记录列表本来也不显示缩略图。
// 所以入库前压掉，只留一行说明；聊天窗口当次显示不受影响。
const INLINE_IMG_RE=/data:image\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/=]+/gi;
function stripInlineImages(src){
  return String(src==null?"":src).replace(INLINE_IMG_RE,"(图片已内联显示，未存入记录)");
}

// 轻量 Markdown 渲染：先整体 HTML 转义防 XSS，再做块级/行内解析。
// 支持：标题、有序/无序列表、引用、分割线、围栏代码块、行内代码、
// 粗体/斜体/删除线、链接、图片。链接仅放行 http/https/相对路径。
// 图片额外放行 data:image/ —— 服务端内联的图片走的就是这条路。
function renderMarkdown(src){
  if(src==null) return "";
  let s=esc(src);

  // 1) 抽取围栏代码块（优先，避免内部内容被后续规则误伤）
  const codeBlocks=[];
  s=s.replace(/```(\w*)\n?([\s\S]*?)```/g,(m,lang,code)=>{
    const idx=codeBlocks.length;
    codeBlocks.push('<pre class="md-pre"><code>'+code.replace(/\n$/,"")+'</code></pre>');
    return "\uE000CODE"+idx+"\uE000";
  });

  // 2) 抽取行内代码
  const inlineCodes=[];
  s=s.replace(/`([^`\n]+?)`/g,(m,code)=>{
    const idx=inlineCodes.length;
    inlineCodes.push('<code class="md-code">'+code+'</code>');
    return "\uE000IC"+idx+"\uE000";
  });

  // 3) 块级解析
  const lines=s.split("\n");
  let html="";
  let inList=null;
  const closeList=()=>{ if(inList){ html+=(inList==="ul"?"</ul>":"</ol>"); inList=null; } };
  const isSpecial=(ln)=>/^(#{1,6})\s/.test(ln)||/^\s*[-*+]\s+/.test(ln)||/^\s*\d+\.\s+/.test(ln)||/^&gt;/.test(ln)||/^\s*([-*_])(\s*\1){2,}\s*$/.test(ln)||/^\uE000CODE\d+\uE000$/.test(ln);
  let i=0;
  while(i<lines.length){
    const line=lines[i];
    let cm=line.match(/^\uE000CODE(\d+)\uE000$/);
    if(cm){ closeList(); html+=codeBlocks[+cm[1]]; i++; continue; }
    let hm=line.match(/^(#{1,6})\s+(.*)$/);
    if(hm){ closeList(); const lvl=hm[1].length; html+="<h"+lvl+' class="md-h md-h'+lvl+'">'+inline(hm[2])+"</h"+lvl+">"; i++; continue; }
    if(/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)){ closeList(); html+='<hr class="md-hr">'; i++; continue; }
    if(/^&gt;\s?/.test(line)){
      closeList();
      let q="";
      while(i<lines.length && /^&gt;\s?/.test(lines[i])){ q+=lines[i].replace(/^&gt;\s?/,"")+"<br>"; i++; }
      html+='<blockquote class="md-quote">'+inline(q.replace(/<br>$/,""))+"</blockquote>";
      continue;
    }
    let ulm=line.match(/^\s*[-*+]\s+(.*)$/);
    let olm=line.match(/^\s*\d+\.\s+(.*)$/);
    if(ulm||olm){
      const type=ulm?"ul":"ol";
      if(inList!==type){ closeList(); html+=(type==="ul"?'<ul class="md-ul">':'<ol class="md-ol">'); inList=type; }
      html+="<li>"+inline(ulm?ulm[1]:olm[1])+"</li>";
      i++; continue;
    }
    if(/^\s*$/.test(line)){ closeList(); i++; continue; }
    closeList();
    let para=line;
    i++;
    while(i<lines.length && !/^\s*$/.test(lines[i]) && !isSpecial(lines[i])){
      para+="<br>"+lines[i];
      i++;
    }
    html+='<p class="md-p">'+inline(para)+"</p>";
  }
  closeList();

  // 4) 还原行内代码
  html=html.replace(/\uE000IC(\d+)\uE000/g,(m,idx)=>inlineCodes[+idx]);
  return html;

  function inline(t){
    // 图片
    t=t.replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g,(m,alt,url)=>{
      if(/^(https?:|\/|#|data:image\/)/i.test(url)) return '<img src="'+url+'" alt="'+alt+'" class="md-img">';
      return m;
    });
    // 链接
    t=t.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g,(m,txt,url)=>{
      if(/^(https?:|\/|#)/i.test(url)) return '<a href="'+url+'" target="_blank" rel="noopener noreferrer" class="md-a">'+txt+"</a>";
      return txt;
    });
    t=t.replace(/\*\*([^*]+?)\*\*/g,"<strong>$1</strong>");
    t=t.replace(/__([^_]+?)__/g,"<strong>$1</strong>");
    t=t.replace(/(^|[^\*])\*([^*\n]+?)\*(?!\*)/g,"$1<em>$2</em>");
    t=t.replace(/(^|[^_])_([^_\n]+?)_(?!_)/g,"$1<em>$2</em>");
    t=t.replace(/~~([^~]+?)~~/g,"<del>$1</del>");
    return t;
  }
}

/* ==================== INIT ==================== */
window.addEventListener("DOMContentLoaded", () => { checkChatAuth(); });
