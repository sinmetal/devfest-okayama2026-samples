document.addEventListener('DOMContentLoaded', async () => {
    const textInput = document.getElementById('text-input');
    const sendButton = document.getElementById('send-button');
    const characterImage = document.getElementById('character-image');
    const characterViewer = document.getElementById('character-viewer');
    const voiceSelect = document.getElementById('voice-select');
    const status = document.getElementById('status');
    const replyText = document.getElementById('reply-text');
    const audio = document.getElementById('speech-audio');
    const audioStatus = document.getElementById('audio-status');
    const playAudio = document.getElementById('play-audio');
    const sources = document.getElementById('sources');
    const searchSuggestions = document.getElementById('search-suggestions');
    const openMouthImg = '/static/images/char-mouth-open.png';
    const closedMouthImg = '/static/images/char-mouth-closed.png';
    // Start loading both frames immediately and retain them for mouth animation.
    const mouthImages = [closedMouthImg, openMouthImg].map(src => {
        const image = new Image();
        image.src = src;
        return image;
    });
    // A missing frame must not prevent speech playback.
    const mouthImagesReady = Promise.allSettled(mouthImages.map(image => image.decode()));
    const history = [];
    let busy = false;
    let lipSyncInterval;
    let audioUrl;

    const stopLipSync = () => {
        clearInterval(lipSyncInterval);
        characterImage.src = closedMouthImg;
        characterViewer.dataset.speaking = 'false';
    };
    audio.addEventListener('playing', () => {
        stopLipSync();
        characterViewer.dataset.speaking = 'true';
        let mouthOpen = false;
        lipSyncInterval = setInterval(() => {
            mouthOpen = !mouthOpen;
            characterImage.src = mouthOpen ? openMouthImg : closedMouthImg;
        }, 150);
    });
    ['pause', 'ended', 'error', 'waiting'].forEach(event => audio.addEventListener(event, stopLipSync));
    // A one-time action only when the browser blocks automatic playback.
    playAudio.addEventListener('click', async () => {
        try {
            await mouthImagesReady;
            await audio.play();
            playAudio.hidden = true;
            audioStatus.textContent = '';
        } catch {
            audioStatus.textContent = '音声を再生できませんでした。ブラウザーの音声設定を確認してください。';
        }
    });
    const updateInput = () => {
        textInput.style.height = 'auto';
        textInput.style.height = `${Math.min(textInput.scrollHeight, 120)}px`;
        sendButton.disabled = busy || !textInput.value.trim();
    };

    try {
        const response = await fetch('/config');
        if (!response.ok) throw new Error('Voice configuration failed');
        const config = await response.json();
        voiceSelect.replaceChildren(...config.voices.map(voice => new Option(config.voice_labels?.[voice] || voice, voice)));
        voiceSelect.value = config.default_voice;
    } catch (error) {
        audioStatus.textContent = '音声設定を取得できませんでした。ページを再読み込みしてください。';
    }

    const post = async (path, body) => {
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), 150000);
        try {
            const response = await fetch(path, {
                method: 'POST', headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body), signal: controller.signal,
            });
            if (!response.ok) {
                const error = await response.json().catch(() => ({}));
                throw new Error(typeof error.detail === 'string' ? error.detail : 'リクエストに失敗しました。');
            }
            return await (path === '/tts' ? response.blob() : response.json());
        } catch (error) {
            if (controller.signal.aborted) {
                throw new Error('応答が時間内に届きませんでした。接続とサーバーのログを確認し、もう一度送信してください。');
            }
            throw error;
        } finally {
            clearTimeout(timer);
        }
    };

    const handleSendMessage = async () => {
        const message = textInput.value.trim();
        if (!message || busy) return;
        const restoreFocus = document.activeElement === textInput;
        busy = true;
        sendButton.disabled = true;
        textInput.disabled = true;
        voiceSelect.disabled = true;
        audio.pause();
        stopLipSync();
        audio.removeAttribute('src');
        playAudio.hidden = true;
        if (audioUrl) URL.revokeObjectURL(audioUrl);
        audioStatus.textContent = '';
        status.textContent = '返答を考えています…';
        status.dataset.busy = 'true';
        status.dataset.error = 'false';
        replyText.textContent = 'うーん、ちょっと考えています…';
        if (sources) sources.replaceChildren();
        if (searchSuggestions) { searchSuggestions.hidden = true; searchSuggestions.srcdoc = ''; }
        let replyReceived = false;
        let displayReply;
        try {
            const data = await post('/chat', { message, history });
            replyReceived = true;
            displayReply = () => {
                // Render model text literally, including any HTML-like content.
                replyText.textContent = data.response;
                replyText.scrollTop = 0;
                status.textContent = '返答しました';
                if (sources) {
                    for (const source of data.sources || []) {
                        let url;
                        try { url = new URL(source.url); } catch { continue; }
                        if (!['https:', 'http:'].includes(url.protocol)) continue;
                        const link = document.createElement('a');
                        link.href = url.href;
                        link.textContent = source.title;
                        link.target = '_blank';
                        link.rel = 'noopener noreferrer';
                        sources.append(link);
                    }
                }
                if (searchSuggestions && data.grounding_html) {
                    searchSuggestions.srcdoc = data.grounding_html;
                    searchSuggestions.hidden = false;
                }
                history.push({ role: 'user', text: message }, { role: 'model', text: data.response });
                history.splice(0, Math.max(0, history.length - 20));
                textInput.value = '';
                updateInput();
            };
            status.textContent = '音声を生成しています…';
            audioStatus.textContent = '音声を生成しています…';
            const speech = await post('/tts', { text: data.response, voice: voiceSelect.value || 'Leda' });
            audioUrl = URL.createObjectURL(speech);
            audio.src = audioUrl;
            displayReply();
            audioStatus.textContent = '';
            try {
                await mouthImagesReady;
                await audio.play();
            } catch {
                audioStatus.textContent = '自動再生がブロックされました。再生ボタンを押してください。';
                playAudio.hidden = false;
            }
        } catch (error) {
            stopLipSync();
            if (replyReceived) {
                displayReply();
                status.textContent = 'テキストで返答しました';
                audioStatus.textContent = `音声を生成できませんでした: ${error.message}`;
            } else {
                status.textContent = error.message;
                status.dataset.error = 'true';
                replyText.textContent = 'うまく返答できませんでした。もう一度、送ってみてください。';
            }
        } finally {
            busy = false;
            status.dataset.busy = 'false';
            updateInput();
            textInput.disabled = false;
            voiceSelect.disabled = false;
            if (restoreFocus && document.activeElement === document.body) textInput.focus({ preventScroll: true });
        }
    };

    sendButton.addEventListener('click', handleSendMessage);
    textInput.addEventListener('keydown', event => {
        if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && event.keyCode !== 229) {
            event.preventDefault();
            handleSendMessage();
        }
    });
    textInput.addEventListener('input', updateInput);
    window.addEventListener('beforeunload', () => {
        audio.pause();
        stopLipSync();
        if (audioUrl) URL.revokeObjectURL(audioUrl);
    });
});
