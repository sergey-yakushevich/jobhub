(() => {
  'use strict';
  const csrf = document.querySelector('meta[name="csrf-token"]')?.content || '';
  try { const theme=localStorage.getItem('jobhub-theme'); if(theme==='light'||theme==='dark') document.documentElement.dataset.theme=theme; } catch (_) {}
  const themeButton=document.querySelector('.theme-toggle');
  const themeLabel=()=>{ if(themeButton) themeButton.textContent=document.documentElement.dataset.theme==='light'?'Dark theme':'Light theme'; };
  themeLabel();
  themeButton?.addEventListener('click',()=>{
    const theme=document.documentElement.dataset.theme==='light'?'dark':'light';
    document.documentElement.dataset.theme=theme;
    try { localStorage.setItem('jobhub-theme',theme); } catch (_) {}
    themeLabel();
  });
  const formats = {
    short: { month:'short', day:'numeric', hour:'2-digit', minute:'2-digit' },
    full: { year:'numeric', month:'short', day:'numeric', hour:'2-digit', minute:'2-digit', timeZoneName:'short' },
    day: { weekday:'long', year:'numeric', month:'long', day:'numeric' },
    time: { hour:'2-digit', minute:'2-digit' }
  };
  for (const el of document.querySelectorAll('time[data-date]')) {
    const date = new Date(el.dateTime);
    if (!Number.isNaN(date.getTime())) { el.textContent = new Intl.DateTimeFormat(undefined, formats[el.dataset.date] || formats.full).format(date); el.title = date.toLocaleString(undefined,formats.full); }
  }
  for (const el of document.querySelectorAll('input[data-iso]')) {
    if (!el.dataset.iso) continue;
    const date = new Date(el.dataset.iso);
    if (!Number.isNaN(date.getTime())) { date.setMinutes(date.getMinutes()-date.getTimezoneOffset()); el.value=date.toISOString().slice(0,16); }
  }
  for (const el of document.querySelectorAll('[data-open]')) el.addEventListener('click', () => document.getElementById(el.dataset.open)?.showModal());
  for (const el of document.querySelectorAll('.close-dialog')) el.addEventListener('click', () => el.closest('dialog')?.close());
  const menu = document.querySelector('.workspace-menu');
  document.addEventListener('click', e => { if (menu && !menu.contains(e.target)) menu.open=false; });
  document.addEventListener('keydown', e => { if (e.key==='Escape' && menu?.open) { menu.open=false; menu.querySelector('summary').focus(); } });
  const chartPanel=document.querySelector('.chart-panel');
  if(chartPanel){
    let series='jobs';
    const readout=chartPanel.querySelector('.chart-readout');
    const reset=()=>{readout.textContent=series==='jobs'?'jobs per day':'events per day';};
    for(const button of chartPanel.querySelectorAll('[data-chart]')) button.addEventListener('click',()=>{
      series=button.dataset.chart;
      for(const chart of chartPanel.querySelectorAll('[data-chart-series]'))chart.toggleAttribute('hidden',chart.dataset.chartSeries!==series);
      for(const option of chartPanel.querySelectorAll('[data-chart]'))option.setAttribute('aria-pressed',String(option===button));
      reset();
    });
    for(const chart of chartPanel.querySelectorAll('.chart')){
      const show=e=>{const col=e.target.closest('.chart-col');if(col)readout.textContent=col.dataset.tip;};
      chart.addEventListener('pointermove',show);chart.addEventListener('pointerdown',show);chart.addEventListener('focusin',show);
      chart.addEventListener('pointerleave',reset);chart.addEventListener('focusout',reset);
    }
  }
  const filters = document.getElementById('job-filters');
  if (filters) {
    const search = filters.elements.q;
    let timer;
    for (const select of filters.querySelectorAll('select')) select.addEventListener('change', () => filters.requestSubmit());
    search.addEventListener('input', e => {
      clearTimeout(timer);
      if (e.isComposing) return;
      timer = setTimeout(() => {
        try { sessionStorage.setItem('jobhub-search-focus', JSON.stringify({query:search.value,position:search.selectionStart})); } catch (_) {}
        filters.requestSubmit();
      }, 400);
    });
    filters.addEventListener('submit', () => clearTimeout(timer));
    try {
      const saved = JSON.parse(sessionStorage.getItem('jobhub-search-focus') || 'null');
      sessionStorage.removeItem('jobhub-search-focus');
      if (saved && saved.query === search.value) { search.focus({preventScroll:true}); search.setSelectionRange(saved.position,saved.position); }
    } catch (_) {}
  }
  const request = async (url, method, body, version) => {
    const headers = {'Content-Type':'application/json','X-CSRF-Token':csrf};
    if (version) headers['If-Match'] = `"${version}"`;
    const res = await fetch(url,{method,headers,body:JSON.stringify(body),credentials:'same-origin'});
    let data; try { data=await res.json(); } catch (_) { throw new Error('The server could not complete this request. Please try again.'); }
    if (!res.ok) {
      if(res.status===401) throw new Error('Your session expired. Sign in again, then retry.');
      if(res.status===409) throw new Error('This record changed, or this source key was already used with different data. Reload the job before editing again.');
      throw new Error(data.error?.message || data.error || 'Unable to save.');
    }
    return data;
  };
  const saveForm = async (form, run) => {
    const error=form.querySelector('.form-error'); const buttons=[...form.querySelectorAll('button[type="submit"]')];
    error.hidden=true;buttons.forEach(b=>b.disabled=true);
    try { await run(); } catch(err) { error.textContent=err.message;error.hidden=false;error.scrollIntoView({block:'nearest'}); } finally {buttons.forEach(b=>b.disabled=false);}
  };
  const jobForm=document.getElementById('job-form');
  const jobURL=id=>`/jobs/${id}`+(new URLSearchParams(location.search).get('profile')==='all'?'?profile=all':'');
  jobForm?.addEventListener('submit', e => {
    e.preventDefault(); saveForm(jobForm,async()=>{
      const f=new FormData(jobForm); const body={};
      for(const name of ['company','title','stage','remote','location','employment','url','currency','salary_period','fit_summary','description','recruiter_name','recruiter_contact','next_action','waiting_on','interview_url']) body[name]=String(f.get(name)||'').trim();
      body.currency=body.currency.toUpperCase();
      for(const name of ['salary_min','salary_max','fit_score']) { const value=String(f.get(name)||'').trim();body[name]=value===''?null:Number(value); }
      for(const name of ['strengths','gaps']) body[name]=String(f.get(name)||'').split('\n').map(x=>x.trim()).filter(Boolean);
      body.skills=String(f.get('skills')||'').split(',').map(x=>x.trim()).filter(Boolean);
      for(const name of ['next_action_at','interview_at']) body[name]=f.get(name)?new Date(String(f.get(name))).toISOString():'';
      body.needs_you=f.has('needs_you');
      const id=jobForm.dataset.jobId;
      if(!id) {body.profile=String(f.get('profile')||'sergey').trim();jobForm.dataset.sourceKey ||= `manual:${crypto.randomUUID()}`;body.source_key=jobForm.dataset.sourceKey;}
      const result=await request(id?`/ui/jobs/${id}`:'/ui/jobs',id?'PATCH':'POST',body,jobForm.dataset.version);
      location.assign(jobURL(result.id));
    });
  });
  const eventForm=document.getElementById('event-form');
  eventForm?.addEventListener('submit', e => {
    const kind=e.submitter?.value || 'note';
    e.preventDefault();saveForm(eventForm,async()=>{
      const f=new FormData(eventForm);const body={};
      for(const name of ['kind','summary','body','actor','channel','direction','contact','source_url','verification'])body[name]=String(f.get(name)||'').trim();
      body.kind=kind;
      eventForm.dataset.sourceKey ||= `manual:${crypto.randomUUID()}`;body.source_key=eventForm.dataset.sourceKey;
      if(f.get('happened_at')) body.happened_at=new Date(String(f.get('happened_at'))).toISOString();
      await request(`/ui/jobs/${eventForm.dataset.jobId}/events`,'POST',body);
      location.assign(jobURL(eventForm.dataset.jobId));
    });
  });
})();
