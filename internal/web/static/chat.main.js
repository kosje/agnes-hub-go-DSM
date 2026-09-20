// chat.main.js - Agnes Chat UI logic

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

function renderLogin(){
  document.getElementById("app").innerHTML=`
  <div class="login">
    <div class="card">
      <h1>Agnes Chat</h1>
      <p class="muted">AI Image & Video Generation</p>
      <div id="toast" class="hide"></div>
      <label>Password</label>
      <input id="pw" type="password" placeholder="admin password">
      <div style="margin-top:12px"><button class="primary" id="btnLogin">Login</button></div>
      <div class="hint">Default: admin123</div>
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
    <h1>Agnes Chat</h1>
    <select id="keySel" class="model-sel" title="API Key"><option value="">Loading keys...</option></select>
    <button class="sm" id="btnRefresh">Refresh</button>
    <button class="sm danger" id="btnLogout">Logout</button>
  </div>
  <div class="chat-layout">
    <div class="chat-main">
      <div class="tabs">
        <button data-tab="chat" class="on" id="tabChat">Chat</button>
        <button data-tab="image" id="tabImage">Image</button>
        <button data-tab="video" id="tabVideo">Video</button>
      </div>
      <div id="chatView" class="msg-list"></div>
      <div id="imageView" class="hide" style="padding:16px;overflow-y:auto;flex:1"></div>
      <div id="videoView" class="hide" style="padding:16px;overflow-y:auto;flex:1"></div>
      <div id="inputArea" class="input-area">
        <div class="input-row">
          <input id="textInput" class="msg-input" placeholder="Type a message..." rows="3">
          <button class="primary send-btn" id="btnSend">Send</button>
        </div>
      </div>
    </div>
    <div class="sidebar">
      <h3>History</h3>
      <div id="historyList"></div>
      <div id="imgHistory" class="hide"></div>
      <div id="vidHistory" class="hide"></div>
    </div>
  </div>
  <div id="toast" class="hide"></div>`;

  // Load keys
  api("/api/keys").then(d=>{
    state.keys=(d.keys||[]).filter(k=>k.enabled);
    const sel=document.getElementById("keySel");
    if(state.keys.length===0){
      sel.innerHTML="<option value=''>No enabled keys</option>";
      toast("No enabled API keys configured","warn");
    }else{
      sel.innerHTML=state.keys.map(k=>`<option value="${esc(k.key)}">${esc(k.name)} (${esc(k.key.slice(0,8))}...)</option>`).join("");
      if(!state.activeKey) state.activeKey=state.keys[0].key;
      sel.value=state.activeKey;
    }
  }).catch(()=>{});

  sel.onchange=e=>{state.activeKey=e.target.value;};

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
  if(state.sending||!state.activeKey)return;
  const input=document.getElementById("textInput");
  const text=input.value.trim();
  if(!text)return;
  input.value="";
  state.sending=true;
  document.getElementById("btnSend").disabled=true;
  document.getElementById("btnSend").innerHTML='<span class="spinner"></span>';

  // Add user message
  const userMsg=addMsg("user",text,null,Date.now()/1000);

  // Add assistant placeholder
  const assMsg=addMsg("ass","",state.model||"agnes-auto",Date.now()/1000);
  const bubble=assMsg.querySelector(".msg-bubble");
  bubble.innerHTML='<span class="spinner"></span> Waiting...';

  try{
    const resp=await api("/v1/chat/completions",{
      method:"POST",
      body:JSON.stringify({
        model:state.model||"agnes-auto",
        messages:[{role:"user",content:text}],
        stream:true
      })
    });
    bubble.textContent="";
    let full="";
    if(resp.choices&&resp.choices[0]){
      // Non-streaming or first chunk
      const delta=resp.choices[0].delta||{};
      full=delta.content||"";
      bubble.textContent=full;
    }else if(resp.choices&&resp.choices[0]&&resp.choices[0].message){
      full=resp.choices[0].message.content||"";
      bubble.textContent=full;
    }
    // Save to history
    state.history.push({id:Date.now(),type:"chat",text:text.substring(0,60),ts:Date.now()/1000,model:state.model||"agnes-auto"});
    renderHistory();
  }catch(e){
    bubble.textContent="Error: "+e.message;
    bubble.style.color="var(--bad)";
  }finally{
    state.sending=false;
    document.getElementById("btnSend").disabled=false;
    document.getElementById("btnSend").textContent="Send";
  }
}

/* ==================== IMAGE GENERATION ==================== */
function renderImageView(){
  const view=document.getElementById("imageView");
  view.innerHTML=`
    <label>Prompt</label>
    <textarea id="imgPrompt" placeholder="Describe the image you want to generate..."></textarea>
    <div style="margin-top:8px;display:flex;gap:8px;align-items:center">
      <select id="imgModel" style="width:200px">
        <option value="agnes-auto">agnes-auto (auto-detect)</option>
        <option value="agnes-image-2.5-flash">agnes-image-2.5-flash</option>
        <option value="agnes-image-2.1-flash">agnes-image-2.1-flash</option>
        <option value="dall-e-3">dall-e-3</option>
      </select>
      <button class="primary" id="btnGenImg">Generate</button>
    </div>
    <div id="imgResult" class="img-grid" style="margin-top:12px"></div>`;
  document.getElementById("btnGenImg").onclick=generateImage;
}

async function generateImage(){
  const prompt=document.getElementById("imgPrompt").value.trim();
  const model=document.getElementById("imgModel").value;
  if(!prompt)return;
  const result=document.getElementById("imgResult");
  result.innerHTML='<div class="muted"><span class="spinner"></span> Generating...</div>';
  try{
    const resp=await api("/v1/images/generations",{
      method:"POST",
      body:JSON.stringify({model,prompt,n:1,size:"1024x1024"})
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
    <label>Prompt</label>
    <textarea id="vidPrompt" placeholder="Describe the video you want to generate..."></textarea>
    <div style="margin-top:8px;display:flex;gap:8px;align-items:center">
      <select id="vidModel" style="width:200px">
        <option value="agnes-auto">agnes-auto (auto-detect)</option>
        <option value="agnes-video-2.5-flash">agnes-video-2.5-flash</option>
        <option value="agnes-video-v2.0">agnes-video-v2.0</option>
      </select>
      <button class="primary" id="btnGenVid">Generate</button>
    </div>
    <div id="vidResult" style="margin-top:12px"></div>`;
  document.getElementById("btnGenVid").onclick=generateVideo;
}

async function generateVideo(){
  const prompt=document.getElementById("vidPrompt").value.trim();
  const model=document.getElementById("vidModel").value;
  if(!prompt)return;
  const result=document.getElementById("vidResult");
  result.innerHTML='<div class="muted"><span class="spinner"></span> Submitting...</div>';
  try{
    const resp=await api("/v1/videos",{
      method:"POST",
      body:JSON.stringify({model,prompt})
    });
    const jobId=resp.job_id||resp.id;
    if(jobId){
      result.innerHTML=`<div class="banner good">Video submitted. Job ID: ${esc(jobId)}</div>`;
      pollVideo(jobId,result);
      state.history.push({id:Date.now(),type:"video",text:prompt.substring(0,60),ts:Date.now()/1000,model,jobs:[jobId]});
      renderHistory();
    }
  }catch(e){
    result.innerHTML=`<div class="banner bad">${esc(e.message)}</div>`;
  }
}

async function pollVideo(jobId,resultEl){
  const poll=async()=>{
    try{
      const resp=await api(`/v1/videos/${jobId}`);
      if(resp.status==="completed"&&resp.video_url){
        resultEl.innerHTML+=`<div class="video-wrap"><video controls src="${esc(resp.video_url)}"></video></div>`;
        return;
      }else if(resp.status==="failed"){
        resultEl.innerHTML+=`<div class="banner bad">Failed: ${esc(resp.error||"unknown")}</div>`;
        return;
      }
      setTimeout(poll,3000);
    }catch(e){
      resultEl.innerHTML+=`<div class="banner warn">Poll error: ${esc(e.message)}</div>`;
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
    imgList.innerHTML=(state.imgJobs.slice(-20).reverse()).map(j=>`
      <div class="history-item" onclick="showImgJob('${esc(j.job_id)}')">
        <div class="muted">${timeAgo(j.created_at)}</div>
        <div>${esc((j.prompt||"").substring(0,40))}</div>
        <div class="tag ok">${esc(j.status)}</div>
      </div>`).join("");
  }
  if(vidList&&state.vidJobs){
    vidList.innerHTML=(state.vidJobs.slice(-20).reverse()).map(j=>`
      <div class="history-item">
        <div class="muted">${timeAgo(j.created_at)}</div>
        <div>${esc((j.prompt||"").substring(0,40))}</div>
        <div class="tag ${j.status==='completed'?'ok':j.status==='failed'?'warn':''}">${esc(j.status)}</div>
      </div>`).join("");
  }
}

async function showImgJob(jobId){
  try{
    const resp=await api(`/api/image-jobs/${jobId}`);
    const view=document.getElementById("imageView");
    view.innerHTML=`
      <button class="sm" onclick="switchTab('image')">← Back</button>
      <h3>Image Job: ${esc(jobId)}</h3>
      <p class="muted">Model: ${esc(resp.model||"?")} · Status: ${esc(resp.status||"?")}</p>
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
window.onload=checkSession;
switchTab("chat");
renderImageView();
renderVideoView();
