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

const state = { tab: "chat", session: null, keys: [], accounts: [], activeKey: null, history: [], curHistoryId: null, messages: [], model: "", sending: false, imJobId: null };

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
      <div class="hint">默认密码: admin123</div>
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
      <div class="hint">默认密码: admin123</div>
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
    <h1><span class="logo">🤖</span>Agnes AI 助手</h1>
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

  document.getElementById("btnSend").onclick=sendChat;
  document.getElementById("textInput").onkeydown=e=>{if(e.key==="Enter"&&!e.shiftKey){e.preventDefault();sendChat();}};

  // 渲染生图/生视频视图（必须在 renderApp 之后调用）
  renderImageView();
  renderVideoView();

  loadHistory();
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
            bubble.textContent=full;
          }catch(e){}
        }
      }
    }else{
      // 非流式 JSON 兜底
      const j=await resp.json();
      const c=(j.choices&&j.choices[0])||{};
      full=(c.message&&c.message.content)||(c.delta&&c.delta.content)||c.text||"";
      bubble.textContent=full;
    }
    if(!full)bubble.textContent="（无内容返回）";
    // Save to history
    state.history.push({id:Date.now(),type:"chat",text:text.substring(0,60),ts:Date.now()/1000,model:state.model||"agnes-auto"});
    renderHistory();
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
              <option value="agnes-auto">agnes-auto (自动选择)</option>
              <option value="agnes-image-2.5-flash">agnes-image-2.5-flash</option>
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
    state.history.push({id:Date.now(),type:"image",text:prompt.substring(0,60),ts:Date.now()/1000,model});
    renderHistory();
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
          <option value="agnes-auto">agnes-auto (自动选择)</option>
          <option value="agnes-video-2.5-flash">agnes-video-2.5-flash</option>
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
      state.history.push({id:Date.now(),type:"video",text:prompt.substring(0,60),ts:Date.now()/1000,model,jobs:[jobId]});
      renderHistory();
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
    const [imgJobs,vidJobs]=await Promise.all([
      api("/api/image-jobs"),
      api("/api/video-jobs")
    ]);
    state.imgJobs=imgJobs.jobs||[];
    state.vidJobs=vidJobs.jobs||[];
    renderHistory();
  }catch(e){console.error(e);}
}

function renderHistory(){
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

/* ==================== UTILS ==================== */
function renderMarkdown(text){
  // Simple markdown: bold, links, code
  let html=esc(text);
  html=html.replace(/\*\*(.*?)\*\*/g,"<b>$1</b>");
  html=html.replace(/\*(.*?)\*/g,"<i>$1</i>");
  html=html.replace(/`(.*?)`/g,"<code style='background:#f0f0f0;padding:1px 4px;border-radius:4px;font-size:13px'>$1</code>");
  html=html.replace(/\[([^\]]+)\]\(([^)]+)\)/g,'<a href="$2" target="_blank" rel="noopener">$1</a>');
  html=html.replace(/\n/g,"<br>");
  return html;
}

/* ==================== INIT ==================== */
window.addEventListener("DOMContentLoaded", () => { checkChatAuth(); });
